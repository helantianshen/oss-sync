package serverbackup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/helantianshen/oss-sync/internal/models"
	"github.com/helantianshen/oss-sync/internal/recycle"
)

type Mirror struct {
	ID        string
	Name      string
	Protocol  string
	Address   string
	Username  string
	Password  string
	LastRun   time.Time
	LastOK    time.Time
	LastError string
	Published map[string]string
	Attempted map[string]bool
}

type mirrorStore interface {
	initialize(context.Context) error
	check(context.Context) error
	put(context.Context, string, *os.File, int64) error
	remove(context.Context, string) error
	close()
}

const (
	WebDAV = "webdav"
	Mount  = "mount"
)

func (m *Manager) mirrorPath(id string) (string, error) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id {
		return "", fs.ErrNotExist
	}
	return filepath.Join(m.dir, "mirrors", id+".json"), nil
}

func (m *Manager) Mirrors() ([]Mirror, error) {
	entries, err := os.ReadDir(filepath.Join(m.dir, "mirrors"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var targets []Mirror
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		target, err := m.readMirror(id)
		if err != nil {
			return nil, err
		}
		target.Password, target.Published, target.Attempted = "", nil, nil
		targets = append(targets, target)
	}
	return targets, nil
}

func (m *Manager) readMirror(id string) (Mirror, error) {
	name, err := m.mirrorPath(id)
	if err != nil {
		return Mirror{}, err
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return Mirror{}, err
	}
	var target Mirror
	if err := json.Unmarshal(data, &target); err != nil {
		return Mirror{}, err
	}
	if target.ID != id {
		return Mirror{}, errors.New("invalid mirror identity")
	}
	return target, nil
}

func (m *Manager) writeMirror(target Mirror) error {
	dest, err := m.mirrorPath(target.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(dest), ".mirror-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := json.NewEncoder(file).Encode(target); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), dest)
}

func (m *Manager) AddMirror(ctx context.Context, target Mirror) error {
	target.ID = uuid.NewString()
	target.Name, target.Address = strings.TrimSpace(target.Name), strings.TrimSpace(target.Address)
	if target.Protocol == Mount {
		target.Username, target.Password = "", ""
	}
	if target.Name == "" || len(target.Name) > 128 || target.Address == "" || len(target.Address) > 2048 {
		return errors.New("invalid mirror name or address")
	}
	remote, err := m.openMirror(target)
	if err != nil {
		return err
	}
	defer remote.close()
	if err := remote.initialize(ctx); err != nil {
		return err
	}
	return m.writeMirror(target)
}

func (m *Manager) DeleteMirror(id string) error {
	m.mirrorsMu.Lock()
	defer m.mirrorsMu.Unlock()
	if m.activeMirrors[id] {
		return errors.New("mirror is running")
	}
	file, err := m.mirrorPath(id)
	if err != nil {
		return err
	}
	return os.Remove(file)
}

func (m *Manager) claimMirror(id string) bool {
	m.mirrorsMu.Lock()
	defer m.mirrorsMu.Unlock()
	if m.activeMirrors[id] {
		return false
	}
	m.activeMirrors[id] = true
	return true
}

func (m *Manager) releaseMirror(id string) {
	m.mirrorsMu.Lock()
	defer m.mirrorsMu.Unlock()
	delete(m.activeMirrors, id)
}

func (m *Manager) RunMirrors(ctx context.Context) error {
	targets, err := m.Mirrors()
	if err != nil {
		return err
	}
	var tasks sync.WaitGroup
	for _, target := range targets {
		tasks.Go(func() { _ = m.SyncMirror(ctx, target.ID) })
	}
	tasks.Wait()
	return nil
}

func (m *Manager) SyncMirror(ctx context.Context, id string) error {
	if !m.claimMirror(id) {
		return errors.New("mirror is running")
	}
	defer m.releaseMirror(id)
	target, err := m.readMirror(id)
	if err != nil {
		return err
	}
	err = m.reconcileMirror(ctx, &target)
	target.LastRun = time.Now().UTC()
	target.LastError = ""
	if err != nil {
		target.LastError = err.Error()
	} else {
		target.LastOK = target.LastRun
	}
	return errors.Join(err, m.writeMirror(target))
}

func (m *Manager) reconcileMirror(ctx context.Context, target *Mirror) error {
	remote, err := m.openMirror(*target)
	if err != nil {
		return err
	}
	defer remote.close()
	if err := remote.check(ctx); err != nil {
		return err
	}
	if !m.TryRead() {
		return errors.New("server backup or restore is in progress")
	}
	want, err := m.mirrorFiles(ctx)
	m.EndRead()
	if err != nil {
		return err
	}
	if target.Attempted == nil {
		target.Attempted = make(map[string]bool)
	}
	// Keep new paths in the deletion journal even if an upload succeeds just before a crash.
	journalChanged := false
	folded := make(map[string]string, len(want))
	for key := range want {
		journalChanged = journalChanged || !target.Attempted[key]
		target.Attempted[key] = true
		folded[strings.ToLower(key)] = key
	}
	if journalChanged {
		if err := m.writeMirror(*target); err != nil {
			return err
		}
	}
	for key := range target.Attempted {
		if !safePath(key) || !(strings.HasPrefix(key, "articles/") || strings.HasPrefix(key, "recycle/")) {
			return errors.New("invalid mirror index")
		}
		replacement, ok := folded[strings.ToLower(key)]
		if ok && replacement != key {
			// A case-only rename aliases the original name on some shares.
			if err := remote.remove(ctx, key); err != nil {
				return fmt.Errorf("rename %s: %w", key, err)
			}
			delete(target.Attempted, key)
			delete(target.Published, replacement)
		}
	}
	next := make(map[string]string, len(want))
	for key, file := range want {
		next[key] = file.Hash
		if file.Hash != "" && target.Published[key] == file.Hash {
			continue
		}
		if err := m.sendMirrorFile(ctx, remote, key, file); err != nil {
			return fmt.Errorf("mirror %s: %w", key, err)
		}
	}
	for key := range target.Attempted {
		if _, ok := want[key]; !ok {
			if err := remote.remove(ctx, key); err != nil {
				return fmt.Errorf("remove %s: %w", key, err)
			}
			delete(target.Attempted, key)
		}
	}
	target.Published = next
	return nil
}

func (m *Manager) mirrorFiles(ctx context.Context) (map[string]models.File, error) {
	root, err := os.OpenRoot(m.dataDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	db := m.db.WithContext(ctx)
	var files []models.File
	if err := db.Where("type = ? AND (is_deleted = ? OR storage_key <> '')", "markdown", false).Find(&files).Error; err != nil {
		return nil, err
	}
	desired := make(map[string]models.File, len(files))
	retention := make(map[string]int)
	folded := make(map[string]string, len(files))
	now := time.Now()
	for _, f := range files {
		if id, err := uuid.Parse(f.VaultID); err != nil || id.String() != f.VaultID || !safePath(f.Path) {
			return nil, errors.New("invalid mirrored article path")
		}
		key := path.Join("articles", f.VaultID, f.Path)
		if f.IsDeleted {
			days, known := retention[f.VaultID]
			if !known {
				var err error
				days, err = recycle.RetentionDays(db, f.VaultID)
				if err != nil {
					return nil, err
				}
				retention[f.VaultID] = days
			}
			if !recycle.CanRestore(f, days, now) {
				continue
			}
			exists, err := storageExists(root, f.StorageKey)
			if err != nil {
				return nil, err
			}
			if !exists {
				continue
			}
			key = path.Join("recycle", f.VaultID, strconv.FormatUint(uint64(f.ID), 10), f.Path)
		}
		lower := strings.ToLower(key)
		if existing, ok := folded[lower]; ok && existing != key {
			return nil, errors.New("mirrored paths differ only by case")
		}
		folded[lower], desired[key] = key, f
	}
	return desired, nil
}

func (m *Manager) sendMirrorFile(ctx context.Context, remote mirrorStore, key string, file models.File) error {
	tmp, err := os.CreateTemp(m.dir, ".mirror-content-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	// Stage and verify the bytes before network I/O so a concurrent edit cannot publish a stale revision.
	if err := m.stageMirrorFile(tmp, file); err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return remote.put(ctx, key, tmp, file.Size)
}

func (m *Manager) stageMirrorFile(tmp *os.File, file models.File) error {
	if !m.TryRead() {
		return errors.New("server backup or restore is in progress")
	}
	defer m.EndRead()
	key := file.StorageKey
	if key == "" {
		key = strconv.FormatUint(uint64(file.UserID), 10) + "/" + file.Path
	}
	if !safePath(key) {
		return errors.New("invalid source path")
	}
	root, err := os.OpenRoot(m.dataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	source, err := root.Open(filepath.FromSlash(key))
	if err != nil {
		return err
	}
	defer source.Close()
	return copyChecked(tmp, source, file.Hash, file.Size)
}
