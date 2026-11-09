// Package serverbackup manages portable server snapshots and article mirrors.
package serverbackup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/filestore"
	"github.com/helantianshen/oss-sync/internal/models"
	"github.com/helantianshen/oss-sync/internal/recycle"
	"github.com/helantianshen/oss-sync/internal/storagequota"
)

type Kind string

const (
	Configuration Kind = "configuration"
	Articles      Kind = "articles"
	Full          Kind = "full"

	// MaxUploadBytes bounds both the compressed upload and the extracted payload.
	MaxUploadBytes = 64 << 30
	maxMetadata    = 64 << 20
	formatVersion  = 1
)

type Archive struct {
	ID        string    `json:"-"`
	Kind      Kind      `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	Size      int64     `json:"-"`
	Format    int       `json:"format"`
	Missing   int       `json:"missing,omitempty"`
}

type snapshot struct {
	Archive
	Vaults         []models.Vault              `json:"vaults"`
	Usernames      map[uint]string             `json:"usernames"`
	System         []models.SystemSetting      `json:"system"`
	UserSettings   []models.UserSetting        `json:"user_settings"`
	VaultSettings  []models.VaultSetting       `json:"vault_settings"`
	PluginSettings []models.VaultPluginSetting `json:"plugin_settings"`
	Files          []models.File               `json:"files"`
	History        []models.FileHistory        `json:"history"`
	Users          []models.User               `json:"users"`
	Members        []models.VaultMember        `json:"members"`
	Shares         []models.Share              `json:"shares"`
	Collaborations []models.Collaboration      `json:"collaborations"`
	Devices        []models.ClientDevice       `json:"devices"`
	DeviceAccess   []models.DeviceVaultAccess  `json:"device_access"`
	DeviceVaults   []models.DeviceVault        `json:"device_vaults"`
	SyncStates     []models.VaultSyncState     `json:"sync_states"`
}

type Manager struct {
	db         *gorm.DB
	dataDir    string
	dir        string
	maxStorage int64
	// HTTP mutations and maintenance tasks share this gate with snapshots and restores.
	gate          sync.RWMutex
	operation     sync.Mutex
	mirrorsMu     sync.Mutex
	activeMirrors map[string]bool
}

func New(db *gorm.DB, dataDir string, maxStorage int64) *Manager {
	return &Manager{db: db, dataDir: dataDir, dir: filepath.Join(dataDir, "server-backups"), maxStorage: maxStorage, activeMirrors: make(map[string]bool)}
}

func (m *Manager) TryRead() bool { return m.gate.TryRLock() }
func (m *Manager) EndRead()      { m.gate.RUnlock() }

func validKind(kind Kind) bool {
	return kind == Configuration || kind == Articles || kind == Full
}

func (a Archive) Name() string {
	return fmt.Sprintf("oss-sync_%s_%s_%s.zip", a.Kind, a.CreatedAt.UTC().Format("20060102T150405Z"), a.ID)
}

func (m *Manager) archivePath(id string) (string, error) {
	if parsed, err := uuid.Parse(id); err != nil || parsed.String() != id {
		return "", fs.ErrNotExist
	}
	return filepath.Join(m.dir, id+".zip"), nil
}

func (m *Manager) Open(id string) (*os.File, Archive, error) {
	path, err := m.archivePath(id)
	if err != nil {
		return nil, Archive{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, Archive{}, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, Archive{}, err
	}
	archive, err := zip.NewReader(file, info.Size())
	if err == nil {
		var header Archive
		err = readHeader(archive, &header)
		if err == nil {
			header.ID, header.Size = id, info.Size()
			return file, header, nil
		}
	}
	file.Close()
	return nil, Archive{}, err
}

func (m *Manager) List() ([]Archive, error) {
	entries, err := os.ReadDir(m.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []Archive
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".zip")
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		file, header, err := m.Open(id)
		if err != nil {
			continue
		}
		file.Close()
		result = append(result, header)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result, nil
}

func (m *Manager) Delete(id string) error {
	path, err := m.archivePath(id)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

func (m *Manager) Create(ctx context.Context, kind Kind) (Archive, error) {
	if !validKind(kind) {
		return Archive{}, errors.New("invalid backup type")
	}
	if !m.operation.TryLock() {
		return Archive{}, errors.New("another backup or restore is in progress")
	}
	defer m.operation.Unlock()
	m.gate.Lock()
	defer m.gate.Unlock()
	return m.create(ctx, kind)
}

func (m *Manager) create(ctx context.Context, kind Kind) (Archive, error) {
	archive := Archive{ID: uuid.NewString(), Kind: kind, CreatedAt: time.Now().UTC(), Format: formatVersion}
	data := snapshot{Archive: archive}
	root, err := os.OpenRoot(m.dataDir)
	if err != nil {
		return Archive{}, err
	}
	defer root.Close()
	db := m.db.WithContext(ctx).Unscoped().Session(&gorm.Session{})
	var users []models.User
	if err := find(db, &data.Vaults, &users); err != nil {
		return Archive{}, err
	}
	data.Usernames = make(map[uint]string, len(users))
	for _, user := range users {
		data.Usernames[user.ID] = user.Username
	}
	if kind != Articles {
		if err := find(db, &data.System, &data.UserSettings, &data.VaultSettings, &data.PluginSettings); err != nil {
			return Archive{}, err
		}
		for i := range data.System {
			data.System[i].JWTSecret = ""
		}
	}
	if kind != Configuration {
		if err := find(db, &data.Files, &data.History, &data.SyncStates); err != nil {
			return Archive{}, err
		}
		daysByVault := make(map[string]int)
		for i := range data.Files {
			f := &data.Files[i]
			if f.IsDeleted && f.StorageKey != "" {
				days, known := daysByVault[f.VaultID]
				if !known {
					var err error
					days, err = recycle.RetentionDays(db, f.VaultID)
					if err != nil {
						return Archive{}, err
					}
					daysByVault[f.VaultID] = days
				}
				if !recycle.CanRestore(*f, days, archive.CreatedAt) {
					f.StorageKey = ""
				} else if exists, err := storageExists(root, f.StorageKey); err != nil {
					return Archive{}, err
				} else if !exists {
					f.StorageKey = ""
					archive.Missing++
				}
			}
		}
		for i := range data.History {
			row := &data.History[i]
			if row.ContentKey == "" {
				continue
			}
			if exists, err := storageExists(root, row.ContentKey); err != nil {
				return Archive{}, err
			} else if !exists {
				row.ContentKey = ""
				archive.Missing++
			}
		}
	}
	if kind == Full {
		data.Users = users
		if err := find(db, &data.Members, &data.Shares, &data.Collaborations, &data.Devices, &data.DeviceAccess, &data.DeviceVaults); err != nil {
			return Archive{}, err
		}
	}
	data.Archive = archive
	if err := data.validate(); err != nil {
		return Archive{}, err
	}
	return m.store(archive, func(file *os.File) error {
		writer := zip.NewWriter(file)
		defer writer.Close()
		if err := writeJSON(writer, "manifest.json", archive); err != nil {
			return err
		}
		if err := writeJSON(writer, "snapshot.json", data); err != nil {
			return err
		}
		for _, f := range data.Files {
			if f.IsDeleted && f.StorageKey == "" {
				continue
			}
			key := f.StorageKey
			if key == "" {
				key = strconv.FormatUint(uint64(f.UserID), 10) + "/" + f.Path
			}
			if err := writeBlob(ctx, writer, root, fileEntry(f.ID), key, f.Hash, f.Size); err != nil {
				return fmt.Errorf("backup %s: %w", f.Path, err)
			}
		}
		for _, row := range data.History {
			if row.ContentKey != "" {
				if err := writeBlob(ctx, writer, root, historyEntry(row.ID), row.ContentKey, "", -1); err != nil {
					return fmt.Errorf("backup history %d: %w", row.ID, err)
				}
			}
		}
		return writer.Close()
	})
}

func find(db *gorm.DB, rows ...any) error {
	for _, dest := range rows {
		if err := db.Session(&gorm.Session{}).Find(dest).Error; err != nil {
			return err
		}
	}
	return nil
}

func fileEntry(id uint) string    { return fmt.Sprintf("files/%d", id) }
func historyEntry(id uint) string { return fmt.Sprintf("history/%d", id) }

func writeJSON(writer *zip.Writer, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maxMetadata {
		return errors.New("backup metadata is too large")
	}
	entry, err := writer.Create(name)
	if err != nil {
		return err
	}
	_, err = entry.Write(data)
	return err
}

func writeBlob(ctx context.Context, writer *zip.Writer, root *os.Root, name, key, hash string, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !safePath(key) {
		return errors.New("invalid storage path")
	}
	source, err := root.Open(filepath.FromSlash(key))
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("storage entry is not a regular file")
	}
	entry, err := writer.Create(name)
	if err != nil {
		return err
	}
	return copyChecked(entry, source, hash, size)
}

func safePath(key string) bool {
	return key != "." && fs.ValidPath(key) && !strings.ContainsAny(key, "\\:\x00") && filepath.IsLocal(filepath.FromSlash(key))
}

func storageExists(root *os.Root, key string) (bool, error) {
	if !safePath(key) {
		return false, errors.New("invalid storage path")
	}
	info, err := root.Stat(filepath.FromSlash(key))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("storage entry is not a regular file")
	}
	return true, nil
}

func copyChecked(dst io.Writer, src io.Reader, hash string, size int64) error {
	sum := sha256.New()
	if hash != "" {
		dst = io.MultiWriter(dst, sum)
	}
	written, err := io.Copy(dst, src)
	if err != nil {
		return err
	}
	if size >= 0 && written != size || hash != "" && hex.EncodeToString(sum.Sum(nil)) != hash {
		return errors.New("content size or hash does not match metadata")
	}
	return nil
}

func (m *Manager) store(a Archive, write func(*os.File) error) (Archive, error) {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return Archive{}, err
	}
	file, err := os.CreateTemp(m.dir, ".archive-")
	if err != nil {
		return Archive{}, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := write(file); err != nil {
		return Archive{}, err
	}
	info, err := file.Stat()
	if err != nil {
		return Archive{}, err
	}
	if info.Size() > MaxUploadBytes {
		return Archive{}, errors.New("backup is too large")
	}
	if err := file.Sync(); err != nil {
		return Archive{}, err
	}
	if err := file.Close(); err != nil {
		return Archive{}, err
	}
	path, _ := m.archivePath(a.ID)
	if err := storagequota.WithinLimit(m.dataDir, m.maxStorage, 0, func() error {
		return os.Rename(file.Name(), path)
	}); err != nil {
		return Archive{}, err
	}
	a.Size = info.Size()
	return a, nil
}

func readHeader(archive *zip.Reader, a *Archive) error {
	if err := readJSON(archive, "manifest.json", a); err != nil {
		return err
	}
	if a.Format != formatVersion || !validKind(a.Kind) || a.CreatedAt.IsZero() {
		return errors.New("unsupported backup format")
	}
	return nil
}

func readJSON(archive *zip.Reader, name string, dest any) error {
	entry, err := archive.Open(name)
	if err != nil {
		return err
	}
	defer entry.Close()
	data, err := io.ReadAll(io.LimitReader(entry, maxMetadata+1))
	if err != nil {
		return err
	}
	if len(data) > maxMetadata {
		return errors.New("backup metadata is too large")
	}
	return json.Unmarshal(data, dest)
}

func (m *Manager) Import(source io.Reader) (Archive, error) {
	a := Archive{ID: uuid.NewString()}
	stored, err := m.store(a, func(file *os.File) error {
		n, err := io.Copy(file, io.LimitReader(source, MaxUploadBytes+1))
		if err != nil {
			return err
		}
		if n > MaxUploadBytes {
			return errors.New("backup is too large")
		}
		archive, err := zip.NewReader(file, n)
		if err != nil {
			return err
		}
		data, blobs, err := inspect(archive)
		if err != nil {
			return err
		}
		specs := data.blobSpecs()
		for name, entry := range blobs {
			spec := specs[name]
			if err := readBlob(entry, io.Discard, spec.hash, spec.size); err != nil {
				return fmt.Errorf("invalid backup content %s: %w", name, err)
			}
		}
		a.Kind, a.Format, a.CreatedAt, a.Missing = data.Kind, data.Format, data.CreatedAt, data.Missing
		return nil
	})
	a.Size = stored.Size
	return a, err
}

type blobSpec struct {
	hash string
	size int64
}

func (s snapshot) blobSpecs() map[string]blobSpec {
	result := make(map[string]blobSpec, len(s.Files)+len(s.History))
	for _, f := range s.Files {
		if !f.IsDeleted || f.StorageKey != "" {
			result[fileEntry(f.ID)] = blobSpec{f.Hash, f.Size}
		}
	}
	for _, row := range s.History {
		if row.ContentKey != "" {
			result[historyEntry(row.ID)] = blobSpec{size: -1}
		}
	}
	return result
}

func inspect(archive *zip.Reader) (snapshot, map[string]*zip.File, error) {
	var header Archive
	if err := readHeader(archive, &header); err != nil {
		return snapshot{}, nil, err
	}
	var data snapshot
	if err := readJSON(archive, "snapshot.json", &data); err != nil {
		return snapshot{}, nil, err
	}
	if data.Format != header.Format || data.Kind != header.Kind || data.Missing < 0 || data.Missing != header.Missing || !data.CreatedAt.Equal(header.CreatedAt) {
		return snapshot{}, nil, errors.New("inconsistent backup metadata")
	}
	specs := data.blobSpecs()
	blobs := make(map[string]*zip.File, len(specs))
	seen := make(map[string]bool, len(archive.File))
	var total uint64
	for _, entry := range archive.File {
		if !entry.Mode().IsRegular() || seen[entry.Name] || entry.UncompressedSize64 > MaxUploadBytes-total {
			return snapshot{}, nil, errors.New("invalid or oversized backup entry")
		}
		seen[entry.Name] = true
		total += entry.UncompressedSize64
		if entry.Name == "manifest.json" || entry.Name == "snapshot.json" {
			continue
		}
		if _, ok := specs[entry.Name]; !ok {
			return snapshot{}, nil, errors.New("unexpected backup entry")
		}
		blobs[entry.Name] = entry
	}
	if len(blobs) != len(specs) {
		return snapshot{}, nil, errors.New("backup content is missing")
	}
	if err := data.validate(); err != nil {
		return snapshot{}, nil, err
	}
	return data, blobs, nil
}

func readBlob(entry *zip.File, dest io.Writer, hash string, size int64) error {
	source, err := entry.Open()
	if err != nil {
		return err
	}
	defer source.Close()
	if size < 0 {
		size = int64(entry.UncompressedSize64)
	}
	return copyChecked(dest, source, hash, size)
}

func (s snapshot) validate() error {
	vaults := make(map[string]uint, len(s.Vaults))
	for _, v := range s.Vaults {
		parsed, err := uuid.Parse(v.ID)
		if err != nil || parsed.String() != v.ID || vaults[v.ID] != 0 || v.OwnerID == 0 {
			return errors.New("invalid backup vault")
		}
		vaults[v.ID] = v.OwnerID
	}
	if s.Kind == Configuration && (len(s.Files) > 0 || len(s.History) > 0) {
		return errors.New("configuration backup contains file content")
	}
	ids := make(map[uint]bool, len(s.Files))
	paths := make(map[string]bool, len(s.Files))
	for _, f := range s.Files {
		key := filestore.VaultStorageKey(f.VaultID, f.Path)
		if f.ID == 0 || ids[f.ID] || paths[key] || vaults[f.VaultID] != f.UserID || f.UserID == 0 || !safePath(f.Path) || f.Size < 0 {
			return errors.New("invalid backup file metadata")
		}
		ids[f.ID], paths[key] = true, true
	}
	clear(ids)
	for _, row := range s.History {
		if row.ID == 0 || ids[row.ID] || vaults[row.VaultID] == 0 || !safePath(row.FilePath) {
			return errors.New("invalid backup history metadata")
		}
		ids[row.ID] = true
	}
	return nil
}
