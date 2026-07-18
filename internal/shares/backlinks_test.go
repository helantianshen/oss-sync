package shares

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/database"
	"github.com/oss/oss-server/internal/models"
)

func setupHandler(t *testing.T) (*Handler, string) {
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
	return New(db, cfg), dataDir
}

func TestCollectBacklinks(t *testing.T) {
	h, dataDir := setupHandler(t)
	userID := uint(1)

	abs := filepath.Join(dataDir, "1", "Notes")
	_ = os.MkdirAll(abs, 0o755)
	_ = os.WriteFile(filepath.Join(abs, "Go.md"), []byte("# Go\n[[Rust]] and [[Missing]]"), 0o644)
	_ = os.WriteFile(filepath.Join(abs, "Rust.md"), []byte("# Rust"), 0o644)

	h.DB.Create(&models.File{UserID: userID, Path: "Notes/Go.md", Type: "markdown", MTime: 1, Hash: "g"})
	h.DB.Create(&models.File{UserID: userID, Path: "Notes/Rust.md", Type: "markdown", MTime: 1, Hash: "r"})

	links, err := h.collectBacklinks(userID, "", "Notes/Go.md")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("expected 1 link, got %d: %v", len(links), links)
	}
	if links[0] != "Notes/Rust.md" {
		t.Errorf("expected Notes/Rust.md, got %q", links[0])
	}
}

func TestCreateOne_SingleFile(t *testing.T) {
	h, _ := setupHandler(t)
	userID := uint(1)
	h.DB.Create(&models.File{UserID: userID, Path: "Note.md", Type: "markdown", MTime: 1, Hash: "x"})

	so, err := h.createOne(userID, "", "Note.md", false, true)
	if err != nil {
		t.Fatalf("createOne: %v", err)
	}
	if len(so.ShareID) != 6 {
		t.Errorf("share_id len=%d", len(so.ShareID))
	}

	var got models.Share
	if err := h.DB.First(&got, "share_id = ?", so.ShareID).Error; err != nil {
		t.Fatalf("share not persisted: %v", err)
	}
	if !got.AllowCopy || got.TargetPath != "Note.md" {
		t.Errorf("unexpected share: %+v", got)
	}
}

func TestCreateOne_MissingFile(t *testing.T) {
	h, _ := setupHandler(t)
	_, err := h.createOne(1, "", "nope.md", false, false)
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestCreateOne_FolderSkipsExistenceCheck(t *testing.T) {
	h, _ := setupHandler(t)
	so, err := h.createOne(1, "", "SomeFolder/", true, false)
	if err != nil {
		t.Fatalf("folder createOne: %v", err)
	}
	if !so.IsFolder {
		t.Error("expected folder")
	}
}
