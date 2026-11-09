package serverbackup_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/helantianshen/oss-sync/internal/config"
	"github.com/helantianshen/oss-sync/internal/database"
	"github.com/helantianshen/oss-sync/internal/history"
	"github.com/helantianshen/oss-sync/internal/models"
	"github.com/helantianshen/oss-sync/internal/reconcile"
	"github.com/helantianshen/oss-sync/internal/recycle"
	"github.com/helantianshen/oss-sync/internal/serverbackup"
)

func TestRecycleAndHistorySurviveReconcileAndArchiveRestore(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Database: config.DatabaseConfig{Driver: "sqlite", DSN: filepath.Join(dir, "test.db")},
		Storage:  config.StorageConfig{DataDir: dir},
	}
	db, err := database.Init(cfg)
	must(t, err)
	conn, err := db.DB()
	must(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	must(t, database.AutoMigrate(db))

	user := models.User{Username: "archive-test", Role: "admin", PasswordHash: "fixture"}
	must(t, db.Create(&user).Error)
	vault := models.Vault{ID: uuid.NewString(), OwnerID: user.ID, Name: "Archive test"}
	must(t, db.Create(&vault).Error)
	content := []byte("deleted markdown")
	file := models.File{
		UserID: user.ID, VaultID: vault.ID, Path: "note.md", Type: "markdown",
		Size: int64(len(content)), Hash: fmt.Sprintf("%x", sha256.Sum256(content)),
		Revision: 10, IsDeleted: true, DeletedAt: sql.NullTime{Time: time.Now(), Valid: true},
	}
	must(t, db.Create(&file).Error)
	file.StorageKey = recycle.Key(vault.ID, file.ID)
	must(t, db.Save(&file).Error)
	recycled := recycle.DiskPath(dir, file)
	must(t, os.MkdirAll(filepath.Dir(recycled), 0o750))
	must(t, os.WriteFile(recycled, content, 0o600))
	key, hash, size, err := history.StoreSnapshot(dir, vault.ID, recycled)
	must(t, err)
	row := models.FileHistory{
		VaultID: vault.ID, FilePath: file.Path, Action: history.ActionDelete,
		Revision: file.Revision, ContentKey: key, Hash: hash, Size: size,
	}
	must(t, db.Create(&row).Error)
	_, err = reconcile.New(db, cfg).Run(true)
	must(t, err)

	manager := serverbackup.New(db, dir, 0)
	archive, err := manager.Create(context.Background(), serverbackup.Full)
	must(t, err)
	if archive.Missing != 0 {
		t.Fatalf("reconciliation removed %d referenced payloads", archive.Missing)
	}
	must(t, os.Remove(recycled))
	must(t, manager.Restore(context.Background(), archive.ID))
	_, err = reconcile.New(db, cfg).Run(true)
	must(t, err)

	var restored models.File
	must(t, db.First(&restored, file.ID).Error)
	body, err := os.ReadFile(recycle.DiskPath(dir, restored))
	must(t, err)
	if !restored.IsDeleted || restored.Revision <= file.Revision || string(body) != string(content) {
		t.Fatalf("recycle entry was not restored: %+v", restored)
	}
	must(t, db.First(&row, row.ID).Error)
	body, err = history.ReadSnapshot(dir, row.ContentKey)
	must(t, err)
	if string(body) != string(content) {
		t.Fatalf("restored history content: %q", body)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
