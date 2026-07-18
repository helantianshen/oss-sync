package reconcile

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/filestore"
	"github.com/oss/oss-server/internal/models"
	"github.com/oss/oss-server/internal/synclock"
)

type Report struct {
	VaultsScanned    int `json:"vaults_scanned"`
	FilesChecked     int `json:"files_checked"`
	FilesRepaired    int `json:"files_repaired"`
	OpenIssues       int `json:"open_issues"`
	FilesQuarantined int `json:"files_quarantined"`
	TempsRemoved     int `json:"temps_removed"`
	DeletedRemoved   int `json:"deleted_content_removed"`
}

func (r Report) String() string {
	return fmt.Sprintf(
		"vaults=%d checked=%d repaired=%d open_issues=%d quarantined=%d temps_removed=%d deleted_removed=%d",
		r.VaultsScanned,
		r.FilesChecked,
		r.FilesRepaired,
		r.OpenIssues,
		r.FilesQuarantined,
		r.TempsRemoved,
		r.DeletedRemoved,
	)
}

type Reconciler struct {
	DB  *gorm.DB
	Cfg *config.Config
	now func() time.Time
}

func New(db *gorm.DB, cfg *config.Config) *Reconciler {
	return &Reconciler{DB: db, Cfg: cfg, now: time.Now}
}

func (r *Reconciler) Run(startup bool) (Report, error) {
	var report Report
	var vaults []models.Vault
	if err := r.DB.Unscoped().Order("created_at asc").Find(&vaults).Error; err != nil {
		return report, err
	}
	for _, vault := range vaults {
		lock := synclock.Vault(vault.ID)
		lock.Lock()
		err := r.reconcileVault(vault, startup, &report)
		lock.Unlock()
		if err != nil {
			return report, fmt.Errorf("reconcile vault %s: %w", vault.ID, err)
		}
		report.VaultsScanned++
	}
	var openIssues int64
	if err := r.DB.Model(&models.StorageIssue{}).
		Where("resolved_at IS NULL AND kind IN ?", []string{"missing", "hash_mismatch"}).
		Count(&openIssues).Error; err != nil {
		return report, err
	}
	report.OpenIssues = int(openIssues)
	return report, nil
}

type diskEntry struct {
	Path      string
	Relative  string
	Size      int64
	ModTime   time.Time
	Temporary bool
	Backup    bool
}

func (r *Reconciler) reconcileVault(
	vault models.Vault,
	startup bool,
	report *Report,
) error {
	var files []models.File
	if err := r.DB.Where("vault_id = ?", vault.ID).Find(&files).Error; err != nil {
		return err
	}
	known := make(map[string]models.File, len(files))
	for _, file := range files {
		known[filepath.Clean(filestore.DiskPath(r.Cfg.Storage.DataDir, file))] = file
	}

	vaultRoot := filepath.Join(r.Cfg.Storage.DataDir, "vaults", vault.ID)
	entries, err := scanVault(vaultRoot)
	if err != nil {
		return err
	}
	hashes := map[string]string{}
	consumed := map[string]bool{}
	now := r.now()

	for _, file := range files {
		if !file.IsDeleted {
			continue
		}
		expected := filepath.Clean(filestore.DiskPath(r.Cfg.Storage.DataDir, file))
		for path := range entries {
			if path != expected && !strings.HasPrefix(path, expected+".backup") {
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			consumed[path] = true
			report.DeletedRemoved++
		}
		if err := r.resolveFileIssues(vault.ID, issueStorageKey(file), now); err != nil {
			return err
		}
	}

	for _, file := range files {
		if file.IsDeleted {
			continue
		}
		report.FilesChecked++
		expected := filepath.Clean(filestore.DiskPath(r.Cfg.Storage.DataDir, file))
		entry, exists := entries[expected]
		if exists && matchesFile(entry, file, hashes) {
			if err := r.resolveFileIssues(vault.ID, issueStorageKey(file), now); err != nil {
				return err
			}
			continue
		}

		candidate := findCandidate(entries, known, consumed, expected, file, hashes)
		if candidate != nil {
			if exists {
				if _, err := r.quarantine(vault.ID, vaultRoot, entry.Path, now); err != nil {
					return err
				}
				report.FilesQuarantined++
				consumed[entry.Path] = true
			}
			if err := os.MkdirAll(filepath.Dir(expected), 0o755); err != nil {
				return err
			}
			if err := os.Rename(candidate.Path, expected); err != nil {
				return err
			}
			consumed[candidate.Path] = true
			report.FilesRepaired++
			if err := r.resolveFileIssues(vault.ID, issueStorageKey(file), now); err != nil {
				return err
			}
			continue
		}

		kind := "missing"
		detail := "database row points to a file that is missing on disk"
		if exists {
			kind = "hash_mismatch"
			detail = "disk content does not match the database hash and no verified backup was found"
		}
		if err := r.recordIssue(vault.ID, file.ID, issueStorageKey(file), kind, detail, now); err != nil {
			return err
		}
	}

	tempAge := time.Duration(r.Cfg.Sync.EffectiveTempFileMaxAgeHours()) * time.Hour
	orphanAge := time.Duration(r.Cfg.Sync.EffectiveOrphanFileGraceHours()) * time.Hour
	for path, entry := range entries {
		if consumed[path] {
			continue
		}
		if _, ok := known[path]; ok {
			continue
		}
		age := now.Sub(entry.ModTime)
		if age < 0 {
			age = 0
		}
		if entry.Backup {
			if !startup && age < tempAge {
				continue
			}
			destination, err := r.quarantine(vault.ID, vaultRoot, path, now)
			if err != nil {
				return err
			}
			report.FilesQuarantined++
			if err := r.recordIssue(
				vault.ID,
				0,
				entry.Relative,
				"orphan_quarantined",
				"stale backup was moved to "+destination,
				now,
			); err != nil {
				return err
			}
			continue
		}
		if entry.Temporary {
			if !startup && age < tempAge {
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			report.TempsRemoved++
			continue
		}
		if !startup && age < orphanAge {
			continue
		}
		destination, err := r.quarantine(vault.ID, vaultRoot, path, now)
		if err != nil {
			return err
		}
		report.FilesQuarantined++
		if err := r.recordIssue(
			vault.ID,
			0,
			entry.Relative,
			"orphan_quarantined",
			"orphan storage object was moved to "+destination,
			now,
		); err != nil {
			return err
		}
	}
	return nil
}

func scanVault(root string) (map[string]diskEntry, error) {
	entries := map[string]diskEntry{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		slashRelative := filepath.ToSlash(relative)
		base := filepath.Base(path)
		entries[filepath.Clean(path)] = diskEntry{
			Path:     filepath.Clean(path),
			Relative: slashRelative,
			Size:     info.Size(),
			ModTime:  info.ModTime(),
			Temporary: strings.HasPrefix(slashRelative, "tmp/") ||
				strings.HasPrefix(base, ".oss-upload-"),
			Backup: strings.Contains(base, ".backup-") || strings.HasSuffix(base, ".backup"),
		}
		return nil
	})
	if os.IsNotExist(err) {
		return entries, nil
	}
	return entries, err
}

func findCandidate(
	entries map[string]diskEntry,
	known map[string]models.File,
	consumed map[string]bool,
	expected string,
	file models.File,
	hashes map[string]string,
) *diskEntry {
	if file.Hash == "" {
		return nil
	}
	for path, entry := range entries {
		if path == expected || consumed[path] {
			continue
		}
		if _, isKnown := known[path]; isKnown && !entry.Backup && !entry.Temporary {
			continue
		}
		if !matchesFile(entry, file, hashes) {
			continue
		}
		copy := entry
		return &copy
	}
	return nil
}

func matchesFile(entry diskEntry, file models.File, hashes map[string]string) bool {
	if entry.Size != file.Size {
		return false
	}
	if file.Hash == "" {
		return true
	}
	hash, ok := hashes[entry.Path]
	if !ok {
		var err error
		hash, err = hashFile(entry.Path)
		if err != nil {
			return false
		}
		hashes[entry.Path] = hash
	}
	return hash == file.Hash
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func (r *Reconciler) quarantine(
	vaultID, vaultRoot, source string,
	now time.Time,
) (string, error) {
	relative, err := filepath.Rel(vaultRoot, source)
	if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
		relative = filepath.Base(source)
	}
	stamp := now.UTC().Format("20060102T150405.000000000Z")
	root := filepath.Join(r.Cfg.Storage.DataDir, "recovery", vaultID, stamp)
	destination := filepath.Join(root, relative)
	for suffix := 1; ; suffix++ {
		if _, statErr := os.Stat(destination); statErr != nil {
			if os.IsNotExist(statErr) {
				break
			}
			return "", statErr
		}
		destination = filepath.Join(root, fmt.Sprintf("%s.%d", relative, suffix))
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(source, destination); err != nil {
		return "", err
	}
	return destination, nil
}

func (r *Reconciler) recordIssue(
	vaultID string,
	fileID uint,
	storageKey, kind, detail string,
	now time.Time,
) error {
	storageKey = truncate(storageKey, 1024)
	var issue models.StorageIssue
	err := r.DB.Where(
		"vault_id = ? AND storage_key = ? AND kind = ?",
		vaultID,
		storageKey,
		kind,
	).First(&issue).Error
	if err == nil {
		return r.DB.Model(&models.StorageIssue{}).Where("id = ?", issue.ID).Updates(map[string]any{
			"file_id":      fileID,
			"detail":       detail,
			"last_seen_at": now,
			"resolved_at":  nil,
		}).Error
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return r.DB.Create(&models.StorageIssue{
		VaultID:     vaultID,
		FileID:      fileID,
		StorageKey:  storageKey,
		Kind:        kind,
		Detail:      detail,
		FirstSeenAt: now,
		LastSeenAt:  now,
	}).Error
}

func (r *Reconciler) resolveFileIssues(vaultID, storageKey string, now time.Time) error {
	return r.DB.Model(&models.StorageIssue{}).
		Where(
			"vault_id = ? AND storage_key = ? AND kind IN ? AND resolved_at IS NULL",
			vaultID,
			truncate(storageKey, 1024),
			[]string{"missing", "hash_mismatch"},
		).
		Update("resolved_at", sql.NullTime{Time: now, Valid: true}).Error
}

func issueStorageKey(file models.File) string {
	if file.StorageKey != "" {
		return file.StorageKey
	}
	return fmt.Sprintf("legacy/%d/%s", file.UserID, filepath.ToSlash(file.Path))
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
