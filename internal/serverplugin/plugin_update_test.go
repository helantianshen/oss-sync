package serverplugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/helantianshen/oss-sync/internal/models"
)

func newUpdateTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "updates.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&models.ServerPlugin{}, &models.ServerPluginAssociation{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Windows 下临时目录清理要求先释放 SQLite 句柄
	t.Cleanup(func() {
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// newUpdateTestManager 只依赖数据库字段，供更新检查逻辑测试复用
func newUpdateTestManager(t *testing.T, db *gorm.DB) *Manager {
	t.Helper()
	return &Manager{db: db}
}

func installUpdateRecord(t *testing.T, db *gorm.DB, id, version, updateURL string, enabled bool) {
	t.Helper()
	manifest := `{"id":"` + id + `","name":"` + id + `","version":"` + version + `","api_version":1,"runtime":"executable","entrypoints":{"linux-amd64":"plugin"}}`
	if updateURL != "" {
		manifest = strings.TrimSuffix(manifest, "}") + `,"update_url":"` + updateURL + `"}`
	}
	record := models.ServerPlugin{
		ID: id, Name: id, Version: version, APIVersion: 1,
		Runtime: "executable", ManifestJSON: manifest, ManifestHash: "hash", Enabled: enabled,
	}
	if err := db.Create(&record).Error; err != nil {
		t.Fatalf("create plugin %s: %v", id, err)
	}
}

func TestCheckPluginUpdatesReportsNewerVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"1.2.0","url":"https://example.com/p.zip","notes":"notes"}`))
	}))
	defer server.Close()

	db := newUpdateTestDB(t)
	installUpdateRecord(t, db, "updatable", "1.0.0", server.URL, true)
	manager := newUpdateTestManager(t, db)

	updates, err := manager.CheckPluginUpdates(context.Background())
	if err != nil {
		t.Fatalf("CheckPluginUpdates() error = %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("updates = %#v, want one entry", updates)
	}
	entry := updates[0]
	if entry.PluginID != "updatable" || entry.Current != "1.0.0" || entry.Latest != "1.2.0" {
		t.Fatalf("entry = %#v", entry)
	}
	if !entry.HasUpdate || !entry.UpdatableHere {
		t.Fatalf("entry flags = %#v, want update and updatable", entry)
	}
}

func TestCheckPluginUpdatesSkipsPluginsWithoutEndpoint(t *testing.T) {
	db := newUpdateTestDB(t)
	installUpdateRecord(t, db, "no-endpoint", "1.0.0", "", true)
	installUpdateRecord(t, db, "disabled-one", "1.0.0", "https://example.invalid/x.json", false)
	manager := newUpdateTestManager(t, db)

	updates, err := manager.CheckPluginUpdates(context.Background())
	if err != nil {
		t.Fatalf("CheckPluginUpdates() error = %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("updates = %#v, want no entry without an endpoint", updates)
	}
}

func TestCheckPluginUpdatesMarksManualWhenNoDownloadURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"2.0.0"}`))
	}))
	defer server.Close()

	db := newUpdateTestDB(t)
	installUpdateRecord(t, db, "manual", "1.0.0", server.URL, true)
	manager := newUpdateTestManager(t, db)

	updates, err := manager.CheckPluginUpdates(context.Background())
	if err != nil {
		t.Fatalf("CheckPluginUpdates() error = %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("updates = %#v, want one entry", updates)
	}
	if !updates[0].HasUpdate || updates[0].UpdatableHere {
		t.Fatalf("entry = %#v, want update but not host-updatable", updates[0])
	}
}

func TestCheckPluginUpdatesIgnoresUnreachableEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	db := newUpdateTestDB(t)
	installUpdateRecord(t, db, "broken", "1.0.0", server.URL, true)
	manager := newUpdateTestManager(t, db)

	updates, err := manager.CheckPluginUpdates(context.Background())
	if err != nil {
		t.Fatalf("CheckPluginUpdates() error = %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("updates = %#v, want one placeholder entry", updates)
	}
	if updates[0].HasUpdate || !updates[0].CheckFailed {
		t.Fatalf("entry = %#v, want an explicit failed check", updates[0])
	}
}

func TestValidPluginUpdateURL(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"https://example.com/plugin.json":   true,
		"http://localhost:8080/plugin.json": true,
		"http://127.0.0.1/plugin.json":      true,
		"http://[::1]/plugin.json":          true,
		"http://example.com/plugin.json":    false,
		"file:///tmp/plugin.json":           false,
		"//example.com/plugin.json":         false,
	}
	for raw, want := range cases {
		if got := validPluginUpdateURL(raw); got != want {
			t.Errorf("validPluginUpdateURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestPluginUpdateRejectsPackageForAnotherPlugin(t *testing.T) {
	manifest, err := json.Marshal(Manifest{
		ID: "other-plugin", Name: "Other plugin", Version: "2.0.0", APIVersion: CurrentAPIVersion,
		Runtime: RuntimeExecutable, Entrypoints: map[string]string{"any": "bin/plugin"},
		Routes: []RouteSpec{{Method: http.MethodGet, Path: "/hello", Public: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	archive := makePackageArchive(t, map[string][]byte{
		"manifest.json": manifest,
		"bin/plugin":    []byte("not executed"),
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	defer server.Close()

	_, err = (&Manager{}).PluginUpdateFromURL(t.Context(), "expected-plugin", "2.0.0", server.URL)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("PluginUpdateFromURL() error = %v, want plugin id mismatch", err)
	}
}

func TestVersionGreaterUsesStrictSemVer(t *testing.T) {
	cases := []struct {
		candidate string
		current   string
		want      bool
	}{
		{"1.0.1", "1.0.0", true},
		{"1.1.0", "1.0.9", true},
		{"2.0.0", "1.99.99", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.0.1", false},
		{"1.0", "1.0.0", false},
		{"nightly", "1.0.0", false},
		{"1.0.0", "nightly", false},
	}
	for _, tc := range cases {
		if got := versionGreater(tc.candidate, tc.current); got != tc.want {
			t.Errorf("versionGreater(%q, %q) = %v, want %v", tc.candidate, tc.current, got, tc.want)
		}
	}
}

func TestInstalledPackageBytesCountsNestedFiles(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(strings.Repeat("a", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "data.bin"), []byte(strings.Repeat("b", 250)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := installedPackageBytes(dir); got != 350 {
		t.Fatalf("installedPackageBytes() = %d, want 350", got)
	}
	if got := installedPackageBytes(dir + "/missing"); got != 0 {
		t.Fatalf("installedPackageBytes(missing) = %d, want 0", got)
	}
}
