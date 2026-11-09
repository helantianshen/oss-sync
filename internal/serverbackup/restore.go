package serverbackup

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/helantianshen/oss-sync/internal/models"
)

func (m *Manager) Restore(ctx context.Context, id string) error {
	file, header, err := m.Open(id)
	if err != nil {
		return err
	}
	defer file.Close()
	archive, err := zip.NewReader(file, header.Size)
	if err != nil {
		return err
	}
	data, blobs, err := inspect(archive)
	if err != nil {
		return err
	}
	if !m.operation.TryLock() {
		return errors.New("another backup or restore is in progress")
	}
	defer m.operation.Unlock()
	m.gate.Lock()
	defer m.gate.Unlock()

	db := m.db.WithContext(ctx)
	if data.Kind == Configuration {
		return db.Transaction(func(tx *gorm.DB) error {
			return restoreSettings(tx.Unscoped().Session(&gorm.Session{}), data)
		})
	}
	root, err := os.OpenRoot(m.dataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	stage := restoreStage{root: root, gen: uuid.NewString(), dirs: make(map[string]bool)}
	committed := false
	defer func() {
		if !committed {
			for dir := range stage.dirs {
				_ = root.RemoveAll(filepath.FromSlash(dir))
			}
		}
	}()
	for i := range data.Files {
		f := &data.Files[i]
		entry := blobs[fileEntry(f.ID)]
		f.StorageKey = ""
		if entry == nil {
			continue
		}
		area := "files"
		if f.IsDeleted {
			area = "recycle"
		}
		f.StorageKey, err = stage.write(ctx, f.VaultID, area, entry, f.Hash, f.Size)
		if err != nil {
			return err
		}
	}
	for i := range data.History {
		row := &data.History[i]
		if entry := blobs[historyEntry(row.ID)]; entry != nil {
			row.ContentKey, err = stage.write(ctx, row.VaultID, "history", entry, "", -1)
			if err != nil {
				return err
			}
		}
	}

	var oldFiles []models.File
	var oldHistory []models.FileHistory
	err = db.Transaction(func(tx *gorm.DB) error {
		tx = tx.Unscoped().Session(&gorm.Session{})
		var states []models.VaultSyncState
		if err := find(tx, &states); err != nil {
			return err
		}
		if data.Kind == Full {
			if err := find(tx, &oldFiles, &oldHistory); err != nil {
				return err
			}
			if err := restoreFull(tx, data); err != nil {
				return err
			}
		} else if err := restoreArticles(tx, &data, &oldFiles, &oldHistory); err != nil {
			return err
		}
		return restoreRevisions(tx, data, states)
	})
	if err != nil {
		return err
	}
	committed = true
	pruneRestoredFiles(root, oldFiles, oldHistory)
	return nil
}

type restoreStage struct {
	root *os.Root
	gen  string
	dirs map[string]bool
}

func (s restoreStage) write(ctx context.Context, vault, area string, entry *zip.File, hash string, size int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir := path.Join("vaults", vault, area, ".oss-restore", s.gen)
	if !s.dirs[dir] {
		if err := s.root.MkdirAll(filepath.FromSlash(dir), 0o700); err != nil {
			return "", err
		}
		s.dirs[dir] = true
	}
	key := path.Join(dir, path.Base(entry.Name))
	file, err := s.root.OpenFile(filepath.FromSlash(key), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if err := readBlob(entry, file, hash, size); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	return key, file.Close()
}

func pruneRestoredFiles(root *os.Root, files []models.File, history []models.FileHistory) {
	remove := func(key string) {
		if safePath(key) {
			if err := root.Remove(filepath.FromSlash(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				log.Printf("[OSS backup] cleanup of replaced content failed: %v", err)
			}
		}
	}
	for _, f := range files {
		key := f.StorageKey
		if key == "" && !f.IsDeleted {
			key = strconv.FormatUint(uint64(f.UserID), 10) + "/" + f.Path
		}
		remove(key)
	}
	for _, row := range history {
		remove(row.ContentKey)
	}
}

func restoreSettings(tx *gorm.DB, data snapshot) error {
	var system models.SystemSetting
	if err := tx.First(&system, 1).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if len(data.System) != 0 {
		setting := data.System[0]
		setting.ID, setting.JWTSecret = 1, system.JWTSecret
		if err := tx.Save(&setting).Error; err != nil {
			return err
		}
	}
	var users []models.User
	var vaults []models.Vault
	if err := find(tx, &users, &vaults); err != nil {
		return err
	}
	usersByName := make(map[string]uint, len(users))
	for _, user := range users {
		usersByName[user.Username] = user.ID
	}
	var userIDs []uint
	for _, username := range data.Usernames {
		if id := usersByName[username]; id != 0 {
			userIDs = append(userIDs, id)
		}
	}
	var userSettings []models.UserSetting
	for _, setting := range data.UserSettings {
		if id := usersByName[data.Usernames[setting.UserID]]; id != 0 {
			setting.UserID = id
			userSettings = append(userSettings, setting)
		}
	}
	if len(userIDs) > 0 {
		if err := tx.Where("user_id IN ?", userIDs).Delete(&models.UserSetting{}).Error; err != nil {
			return err
		}
	}
	localVaults := make(map[string]bool, len(vaults))
	for _, v := range vaults {
		localVaults[v.ID] = true
	}
	var ids []string
	for _, v := range data.Vaults {
		if localVaults[v.ID] {
			ids = append(ids, v.ID)
		}
	}
	var vaultSettings []models.VaultSetting
	var pluginSettings []models.VaultPluginSetting
	for _, setting := range data.VaultSettings {
		if localVaults[setting.VaultID] {
			vaultSettings = append(vaultSettings, setting)
		}
	}
	for _, setting := range data.PluginSettings {
		if localVaults[setting.VaultID] {
			pluginSettings = append(pluginSettings, setting)
		}
	}
	if len(ids) > 0 {
		if err := tx.Where("vault_id IN ?", ids).Delete(&models.VaultSetting{}).Error; err != nil {
			return err
		}
		if err := tx.Where("vault_id IN ?", ids).Delete(&models.VaultPluginSetting{}).Error; err != nil {
			return err
		}
	}
	return insertRows(tx, userSettings, vaultSettings, pluginSettings)
}

func restoreArticles(tx *gorm.DB, data *snapshot, oldFiles *[]models.File, oldHistory *[]models.FileHistory) error {
	if len(data.Vaults) == 0 {
		return nil
	}
	ids := make([]string, 0, len(data.Vaults))
	for _, vault := range data.Vaults {
		ids = append(ids, vault.ID)
	}
	var vaults []models.Vault
	if err := tx.Where("id IN ?", ids).Find(&vaults).Error; err != nil {
		return err
	}
	if len(vaults) != len(ids) {
		return errors.New("article backup requires the original vaults; restore a full backup to recreate them")
	}
	owners := make(map[string]uint, len(vaults))
	for _, v := range vaults {
		owners[v.ID] = v.OwnerID
	}
	if err := tx.Where("vault_id IN ?", ids).Find(oldFiles).Error; err != nil {
		return err
	}
	if err := tx.Where("vault_id IN ?", ids).Find(oldHistory).Error; err != nil {
		return err
	}
	kept := make(map[uint]bool)
	previous := make(map[string]uint, len(*oldFiles))
	for _, f := range *oldFiles {
		previous[f.VaultID+"/"+f.Path] = f.ID
	}
	var nextFileID, nextHistoryID uint
	if err := tx.Model(&models.File{}).Select("COALESCE(MAX(id), 0)").Scan(&nextFileID).Error; err != nil {
		return err
	}
	if err := tx.Model(&models.FileHistory{}).Select("COALESCE(MAX(id), 0)").Scan(&nextHistoryID).Error; err != nil {
		return err
	}
	for i := range data.Files {
		f := &data.Files[i]
		f.UserID = owners[f.VaultID]
		if id := previous[f.VaultID+"/"+f.Path]; id != 0 {
			f.ID = id
			kept[id] = true
		} else {
			nextFileID++
			f.ID = nextFileID
		}
	}
	for i := range data.History {
		nextHistoryID++
		data.History[i].ID = nextHistoryID
	}
	for _, f := range *oldFiles {
		if !kept[f.ID] {
			if err := tx.Where("vault_id = ? AND file_id = ?", f.VaultID, f.ID).Delete(&models.Collaboration{}).Error; err != nil {
				return err
			}
		}
	}
	if err := tx.Where("vault_id IN ?", ids).Delete(&models.File{}).Error; err != nil {
		return err
	}
	if err := tx.Where("vault_id IN ?", ids).Delete(&models.FileHistory{}).Error; err != nil {
		return err
	}
	if err := tx.Where("vault_id IN ?", ids).Delete(&models.StorageIssue{}).Error; err != nil {
		return err
	}
	return insertRows(tx, data.Files, data.History)
}

func restoreFull(tx *gorm.DB, data snapshot) error {
	var current []models.User
	if err := tx.Find(&current).Error; err != nil {
		return err
	}
	currentVersions := make(map[uint]uint, len(current))
	for _, user := range current {
		currentVersions[user.ID] = user.TokenVersion
	}
	admin := false
	for i := range data.Users {
		user := &data.Users[i]
		admin = admin || (user.Role == "admin" && !user.DeletedAt.Valid && user.PasswordHash != "")
		user.TokenVersion = max(currentVersions[user.ID], user.TokenVersion) + 1
	}
	if !admin {
		return errors.New("full backup has no active administrator")
	}
	for _, model := range []any{
		&models.StorageIssue{}, &models.Collaboration{}, &models.Share{}, &models.FileHistory{}, &models.File{},
		&models.DeviceVaultAccess{}, &models.DeviceVault{}, &models.ClientDevice{}, &models.VaultPluginSetting{},
		&models.VaultSyncState{}, &models.VaultSetting{}, &models.VaultMember{}, &models.Vault{},
		&models.UserSetting{}, &models.User{},
	} {
		if err := tx.Where("1 = 1").Delete(model).Error; err != nil {
			return err
		}
	}
	if err := insertRows(tx, data.Users, data.Vaults, data.Members, data.Files, data.History, data.Shares,
		data.Collaborations, data.Devices, data.DeviceAccess, data.DeviceVaults); err != nil {
		return err
	}
	return restoreSettings(tx, data)
}

func restoreRevisions(tx *gorm.DB, data snapshot, existing []models.VaultSyncState) error {
	states := make(map[string]*models.VaultSyncState, len(data.Vaults))
	for _, vault := range data.Vaults {
		base := time.Now().UnixMilli()
		states[vault.ID] = &models.VaultSyncState{VaultID: vault.ID, HeadRevision: base, CompactedRevision: base}
	}
	for _, state := range append(existing, data.SyncStates...) {
		if v := states[state.VaultID]; v != nil {
			if state.HeadRevision >= 1<<52 {
				return errors.New("invalid backup revision")
			}
			v.CompactedRevision = max(v.CompactedRevision, state.HeadRevision+1)
			v.HeadRevision = v.CompactedRevision
		}
	}
	sizes := make(map[string]int64)
	for _, file := range data.Files {
		state := states[file.VaultID]
		state.HeadRevision++
		if !file.IsDeleted {
			sizes[file.VaultID] += file.Size
		}
		if err := tx.Model(&models.File{}).Where("id = ?", file.ID).Updates(map[string]any{
			"revision": state.HeadRevision, "last_writer_client_id": "", "last_operation_id": "",
		}).Error; err != nil {
			return err
		}
	}
	for id, state := range states {
		if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(state).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.DeviceVault{}).Where("vault_id = ?", id).Update("last_cursor", 0).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.Vault{}).Where("id = ?", id).UpdateColumn("storage_used", sizes[id]).Error; err != nil {
			return err
		}
	}
	return nil
}

// Use explicit values so zero-valued settings do not become schema defaults.
func insertRows(tx *gorm.DB, groups ...any) error {
	for _, group := range groups {
		values := reflect.ValueOf(group)
		if values.Len() == 0 {
			continue
		}
		model := reflect.New(values.Type().Elem()).Interface()
		stmt := &gorm.Statement{DB: tx}
		if err := stmt.Parse(model); err != nil {
			return err
		}
		for start := 0; start < values.Len(); start += 50 {
			end := min(start+50, values.Len())
			rows := make([]map[string]any, 0, end-start)
			for i := start; i < end; i++ {
				row := make(map[string]any, len(stmt.Schema.Fields))
				for _, field := range stmt.Schema.Fields {
					if field.DBName != "" {
						value, _ := field.ValueOf(tx.Statement.Context, values.Index(i))
						row[field.DBName] = value
					}
				}
				rows = append(rows, row)
			}
			if err := tx.Model(model).Create(&rows).Error; err != nil {
				return err
			}
		}
		if tx.Dialector.Name() == "postgres" {
			for _, field := range stmt.Schema.PrimaryFields {
				if field.AutoIncrement {
					query := fmt.Sprintf("SELECT setval(pg_get_serial_sequence(?, ?), COALESCE(MAX(%s), 1), COUNT(*) > 0) FROM %s",
						stmt.Quote(field.DBName), stmt.Quote(stmt.Table))
					if err := tx.Exec(query, stmt.Table, field.DBName).Error; err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
