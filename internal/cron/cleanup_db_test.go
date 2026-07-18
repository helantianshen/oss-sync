package cron

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/database"
	"github.com/oss/oss-server/internal/models"
)

func sqlNullTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: true}
}

func setupCleanup(t *testing.T, fixedNow time.Time) (*Cleanup, *gorm.DB, string) {
	t.Helper()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "t.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Storage: config.StorageConfig{DataDir: dataDir}}
	c := NewCleanup(db, cfg)
	c.now = func() time.Time { return fixedNow }
	return c, db, dataDir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeRecycleBin_ExpiredDeleted(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, db, dataDir := setupCleanup(t, now)

	db.Create(&models.User{ID: 1, Username: "u"})
	days := 30
	db.Create(&models.UserSetting{UserID: 1, RecycleBinDays: &days})

	old := now.AddDate(0, 0, -31)
	filePath := "Notes/Old.md"
	abs := filepath.Join(dataDir, "1", "Notes", "Old.md")
	writeFile(t, abs, "old content")
	db.Create(&models.File{
		UserID: 1, Path: filePath, Type: "markdown", Hash: "h", MTime: 1,
		IsDeleted: true, DeletedAt: sqlNullTime(old),
	})

	if err := c.PurgeRecycleBin(); err != nil {
		t.Fatalf("purge: %v", err)
	}

	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Errorf("expected disk file removed, got err=%v", err)
	}
	var cnt int64
	db.Unscoped().Model(&models.File{}).Where("path = ?", filePath).Count(&cnt)
	if cnt != 0 {
		t.Errorf("expected DB row hard-deleted, got %d", cnt)
	}
}

func TestPurgeRecycleBin_NotYetExpired(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, db, dataDir := setupCleanup(t, now)

	db.Create(&models.User{ID: 1, Username: "u"})
	days := 30
	db.Create(&models.UserSetting{UserID: 1, RecycleBinDays: &days})

	recent := now.AddDate(0, 0, -5)
	filePath := "Notes/Recent.md"
	abs := filepath.Join(dataDir, "1", "Notes", "Recent.md")
	writeFile(t, abs, "recent")
	db.Create(&models.File{
		UserID: 1, Path: filePath, Type: "markdown", Hash: "h", MTime: 1,
		IsDeleted: true, DeletedAt: sqlNullTime(recent),
	})

	if err := c.PurgeRecycleBin(); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Errorf("expected file kept (not expired), got err=%v", err)
	}
	var cnt int64
	db.Unscoped().Model(&models.File{}).Where("path = ?", filePath).Count(&cnt)
	if cnt != 1 {
		t.Errorf("expected row kept, got %d", cnt)
	}
}

func TestPurgeRecycleBin_ZeroDaysImmediate(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, db, dataDir := setupCleanup(t, now)

	db.Create(&models.User{ID: 1, Username: "u"})
	zero := 0
	db.Create(&models.UserSetting{UserID: 1, RecycleBinDays: &zero})

	abs := filepath.Join(dataDir, "1", "x.md")
	writeFile(t, abs, "x")
	db.Create(&models.File{
		UserID: 1, Path: "x.md", Type: "markdown", Hash: "h", MTime: 1,
		IsDeleted: true, DeletedAt: sqlNullTime(now),
	})

	if err := c.PurgeRecycleBin(); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Errorf("expected immediate purge, got err=%v", err)
	}
}

func TestPurgeOrphanAttachments_RemovesUnreferencedOld(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, db, dataDir := setupCleanup(t, now)

	db.Create(&models.User{ID: 1, Username: "u"})

	mdPath := "Notes/Doc.md"
	mdAbs := filepath.Join(dataDir, "1", "Notes", "Doc.md")
	writeFile(t, mdAbs, "# Doc\n![kept](kept.png)")
	db.Create(&models.File{UserID: 1, Path: mdPath, Type: "markdown", Hash: "h", MTime: 1})

	keptAbs := filepath.Join(dataDir, "1", "Notes", "kept.png")
	writeFile(t, keptAbs, "img")
	setFileMtime(t, keptAbs, now.Add(-48*time.Hour))
	db.Create(&models.File{UserID: 1, Path: "Notes/kept.png", Type: "attachment", Hash: "k", MTime: 1})

	orphanAbs := filepath.Join(dataDir, "1", "orphan.png")
	writeFile(t, orphanAbs, "img")
	setFileMtime(t, orphanAbs, now.Add(-48*time.Hour))
	db.Create(&models.File{UserID: 1, Path: "orphan.png", Type: "attachment", Hash: "o", MTime: 1})

	if err := c.PurgeOrphanAttachments(); err != nil {
		t.Fatalf("purge orphans: %v", err)
	}

	if _, err := os.Stat(keptAbs); err != nil {
		t.Errorf("referenced attachment should be kept, got err=%v", err)
	}
	if _, err := os.Stat(orphanAbs); !os.IsNotExist(err) {
		t.Errorf("orphan should be removed, got err=%v", err)
	}
	var cnt int64
	db.Unscoped().Model(&models.File{}).Where("path = ?", "orphan.png").Count(&cnt)
	if cnt != 0 {
		t.Errorf("orphan DB row should be deleted, got %d", cnt)
	}
}

func TestPurgeOrphanAttachments_GracePeriodKeepsNew(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, db, dataDir := setupCleanup(t, now)

	db.Create(&models.User{ID: 1, Username: "u"})
	mdAbs := filepath.Join(dataDir, "1", "Doc.md")
	writeFile(t, mdAbs, "# Doc")
	db.Create(&models.File{UserID: 1, Path: "Doc.md", Type: "markdown", Hash: "h", MTime: 1})

	orphanAbs := filepath.Join(dataDir, "1", "new.png")
	writeFile(t, orphanAbs, "img")
	setFileMtime(t, orphanAbs, now.Add(-1*time.Hour))
	db.Create(&models.File{UserID: 1, Path: "new.png", Type: "attachment", Hash: "n", MTime: 1})

	if err := c.PurgeOrphanAttachments(); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := os.Stat(orphanAbs); err != nil {
		t.Errorf("new orphan within 24h grace should be kept, got err=%v", err)
	}
}

func TestScheduler_RegisterAndStop(t *testing.T) {
	c, db, _ := setupCleanup(t, time.Now())
	_ = c
	s := NewScheduler(db, &config.Config{Storage: config.StorageConfig{DataDir: t.TempDir()}})
	s.Register()
	s.Start()
	if err := s.Stop(nil); err != nil {
		t.Errorf("stop: %v", err)
	}
}

func setFileMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}
