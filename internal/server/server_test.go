package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/database"
	"github.com/oss/oss-server/internal/filestore"
	"github.com/oss/oss-server/internal/models"
)

func newTestServer(t *testing.T) (*Server, *gorm.DB, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "test.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := database.AutoMigrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		Server:   config.ServerConfig{Host: "127.0.0.1", Port: 0, Mode: gin.TestMode, MaxMultipartMemoryMB: 8, MaxFileSizeMB: 100},
		Database: config.DatabaseConfig{Driver: "sqlite", DSN: dbPath},
		Storage:  config.StorageConfig{DataDir: dataDir},
		Auth:     config.AuthConfig{JWTSecret: "test-secret", JWTTTLHours: 1},
		Sync:     config.SyncConfig{MaxConcurrency: 6},
	}
	srv, err := New(cfg, db)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, db, dataDir
}

func doJSON(t *testing.T, router *gin.Engine, method, path string, token string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func registerAndLogin(t *testing.T, router *gin.Engine, user, pass string) string {
	t.Helper()
	code, body := doJSON(t, router, "POST", "/api/auth/register", "", map[string]string{
		"username": user,
		"password": pass,
	})
	if code != http.StatusOK {
		t.Fatalf("register: %d %v", code, body)
	}
	return body["token"].(string)
}

func uploadFile(t *testing.T, router *gin.Engine, token, path, content string, mtime int64) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("path", path)
	_ = mw.WriteField("mtime", intToStr(mtime))
	fw, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte(content))
	mw.Close()

	req := httptest.NewRequest("POST", "/api/sync/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func uploadRawFile(t *testing.T, router *gin.Engine, token, path, content string, mtime int64) (int, map[string]any) {
	t.Helper()
	url := "/api/sync/upload?path=" + url.QueryEscape(path) + "&mtime=" + strconv.FormatInt(mtime, 10)
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(content))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func intToStr(n int64) string {
	return strconv.FormatInt(n, 10)
}

func TestFullSyncFlow(t *testing.T) {
	srv, db, dataDir := newTestServer(t)
	router := srv.Router()

	token := registerAndLogin(t, router, "alice", "password123")

	code, body := uploadFile(t, router, token, "Notes/Go.md", "# Go\nHello [[World]]", 1700000000000)
	if code != http.StatusOK {
		t.Fatalf("upload: %d %v", code, body)
	}
	if body["hash"] == "" || body["path"] != "Notes/Go.md" {
		t.Errorf("upload response unexpected: %v", body)
	}

	var stored models.File
	if err := db.Where("user_id = ? AND path = ?", 1, "Notes/Go.md").First(&stored).Error; err != nil {
		t.Fatalf("query uploaded file: %v", err)
	}
	abs := filestore.DiskPath(dataDir, stored)
	if _, err := os.Stat(abs); err != nil {
		t.Errorf("disk file missing: %v", err)
	}

	code, body = doJSON(t, router, "POST", "/api/sync/check", token, map[string]any{
		"mode": "full",
		"files": []map[string]any{
			{"path": "Notes/Go.md", "mtime": 1700000000000, "hash": body["hash"]},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("check: %d %v", code, body)
	}
	serverTime, ok := body["server_time"].(float64)
	if !ok || serverTime <= 0 {
		t.Errorf("server_time missing: %v", body["server_time"])
	}
	results := body["results"].([]any)
	status := results[0].(map[string]any)["status"]
	if status != "in_sync" {
		t.Errorf("expected in_sync, got %v", status)
	}

	code, body = doJSON(t, router, "POST", "/api/sync/check", token, map[string]any{
		"mode": "full",
		"files": []map[string]any{
			{"path": "Notes/Go.md", "mtime": 1700000000000 - 1000, "hash": "different"},
		},
	})
	results = body["results"].([]any)
	status = results[0].(map[string]any)["status"]
	if status != "conflict_detected" {
		t.Errorf("expected conflict_detected, got %v", status)
	}

	req := httptest.NewRequest("GET", "/api/sync/download?path=Notes/Go.md", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("download: %d", w.Code)
	}
	if w.Body.String() != "# Go\nHello [[World]]" {
		t.Errorf("download content mismatch: %q", w.Body.String())
	}
	if w.Header().Get("X-OSS-Hash") == "" {
		t.Error("missing X-OSS-Hash header")
	}

	code, _ = doJSON(t, router, "POST", "/api/sync/delete", token, map[string]string{"path": "Notes/Go.md"})
	if code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Errorf("deleted content remains on disk: %v", err)
	}
	req = httptest.NewRequest("GET", "/api/sync/download?path=Notes/Go.md", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusGone {
		t.Errorf("download after delete: %d", w.Code)
	}
}

func TestRawBinaryUploadFlow(t *testing.T) {
	srv, _, _ := newTestServer(t)
	router := srv.Router()
	token := registerAndLogin(t, router, "rawupload", "password123")

	code, body := uploadRawFile(t, router, token, "附件/测试.bin", "binary\x00content", 1700000000000)
	if code != http.StatusOK {
		t.Fatalf("raw upload: status=%d body=%v", code, body)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sync/download?path="+url.QueryEscape("附件/测试.bin"), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "binary\x00content" {
		t.Errorf("raw download: status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestUploadUnsafePath(t *testing.T) {
	srv, _, _ := newTestServer(t)
	router := srv.Router()
	token := registerAndLogin(t, router, "frank", "password123")

	cases := []string{"../escape.md", "/etc/passwd"}
	for _, p := range cases {
		code, _ := uploadFile(t, router, token, p, "x", 1700000000000)
		if code != http.StatusBadRequest {
			t.Errorf("path %q: expected 400, got %d", p, code)
		}
	}
}

func TestHealthzAndReadyz(t *testing.T) {
	srv, _, _ := newTestServer(t)
	router := srv.Router()

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("healthz: %d", w.Code)
	}

	req = httptest.NewRequest("GET", "/readyz", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("readyz: %d", w.Code)
	}
}

func TestReadyzReportsUnresolvedStorageIssues(t *testing.T) {
	srv, db, _ := newTestServer(t)
	issue := models.StorageIssue{
		VaultID:     "vault-a",
		StorageKey:  "vaults/vault-a/files/Missing.md",
		Kind:        "missing",
		Detail:      "missing",
		FirstSeenAt: time.Now(),
		LastSeenAt:  time.Now(),
	}
	if err := db.Create(&issue).Error; err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with issue=%d body=%s", response.Code, response.Body.String())
	}
	if err := db.Model(&models.StorageIssue{}).Where("id = ?", issue.ID).
		Update("resolved_at", time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response = httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("readyz after resolution=%d body=%s", response.Code, response.Body.String())
	}
}
