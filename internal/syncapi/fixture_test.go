package syncapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/config"
	"github.com/helantianshen/oss-sync/internal/database"
	"github.com/helantianshen/oss-sync/internal/deviceauth"
	"github.com/helantianshen/oss-sync/internal/jwt"
	"github.com/helantianshen/oss-sync/internal/models"
)

type syncFixture struct {
	t      *testing.T
	db     *gorm.DB
	cfg    *config.Config
	router *gin.Engine
	user   models.User
	vault  models.Vault
	token  string
}

// 每个场景独占磁盘数据库与正文目录，清理时先关闭连接再移除临时文件
func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{
		Database: config.DatabaseConfig{Driver: "sqlite", DSN: filepath.Join(root, "sync.sqlite")},
		Storage:  config.StorageConfig{DataDir: filepath.Join(root, "storage")},
		Server:   config.ServerConfig{MaxFileSizeMB: 1},
		Auth:     config.AuthConfig{JWTSecret: "integration-test-only-secret"},
	}
	db, err := database.Init(cfg)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get test database connection: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	if err := database.AutoMigrate(db); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	f := &syncFixture{t: t, db: db, cfg: cfg, router: gin.New()}
	f.user = models.User{Username: "owner", PasswordHash: "unused", Role: "user"}
	f.create(&f.user)
	f.vault = models.Vault{ID: "owner-vault", OwnerID: f.user.ID, Name: "Owner vault"}
	f.create(&f.vault)
	f.create(&models.VaultSyncState{VaultID: f.vault.ID})
	f.create(&models.SystemSetting{ID: 1, DefaultRecycleBinDays: 30})
	f.create(&models.ClientDevice{UserID: f.user.ID, ClientID: "owner-device", Status: deviceauth.DeviceStatusApproved})
	f.grant(f.user.ID, "owner-device", f.vault.ID)
	f.token = f.sign(f.user, "owner-device")
	New(db, cfg).Register(f.router)
	return f
}

func (f *syncFixture) create(value any) {
	f.t.Helper()
	if err := f.db.Create(value).Error; err != nil {
		f.t.Fatalf("create fixture record: %v", err)
	}
}

func (f *syncFixture) grant(userID uint, clientID, vaultID string) {
	f.t.Helper()
	f.create(&models.DeviceVaultAccess{
		UserID: userID, ClientID: clientID, VaultID: vaultID,
		GrantedByUserID: f.user.ID, GrantedAt: time.Unix(1700000000, 0),
	})
}

func (f *syncFixture) sign(user models.User, clientID string) string {
	f.t.Helper()
	token, err := jwt.Sign(f.cfg.Auth.JWTSecret, jwt.Claims{
		UserID: user.ID, Username: user.Username, Role: user.Role,
		TokenVersion: user.TokenVersion, DeviceID: jwt.DeviceID(clientID),
	}, time.Hour)
	if err != nil {
		f.t.Fatalf("sign device token: %v", err)
	}
	return token
}

func (f *syncFixture) request(method, target, token, contentType string, body []byte) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, req)
	return response
}

func (f *syncFixture) endpoint(suffix string) string {
	return "/api/vaults/" + f.vault.ID + "/sync/" + suffix
}

func uploadTarget(vaultID, path string, base int64, operation string) string {
	query := url.Values{
		"path": {path}, "base_revision": {strconv.FormatInt(base, 10)},
		"operation_id": {operation}, "mtime": {"1700000000123"},
	}
	return "/api/vaults/" + vaultID + "/sync/upload?" + query.Encode()
}

func (f *syncFixture) upload(path, content string, base int64, operation string) V2FileMeta {
	f.t.Helper()
	response := f.request(http.MethodPost, uploadTarget(f.vault.ID, path, base, operation), f.token, "application/octet-stream", []byte(content))
	return decodeResponse[V2FileMeta](f.t, response, http.StatusOK)
}

func (f *syncFixture) postJSON(target string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatalf("encode request: %v", err)
	}
	return f.request(http.MethodPost, target, f.token, "application/json", raw)
}

func decodeResponse[T any](t *testing.T, response *httptest.ResponseRecorder, status int) T {
	t.Helper()
	if response.Code != status {
		t.Fatalf("HTTP status = %d, want %d; body: %s", response.Code, status, response.Body.String())
	}
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, response.Body.String())
	}
	return value
}

func (f *syncFixture) changes(after int64, limit int) v2ManifestResponse {
	f.t.Helper()
	target := f.endpoint("changes") + "?after=" + strconv.FormatInt(after, 10) + "&limit=" + strconv.Itoa(limit)
	return decodeResponse[v2ManifestResponse](f.t, f.request(http.MethodGet, target, f.token, "", nil), http.StatusOK)
}

func (f *syncFixture) history(path string) []historyOut {
	f.t.Helper()
	response := f.request(http.MethodGet, f.endpoint("history")+"?path="+url.QueryEscape(path), f.token, "", nil)
	return decodeResponse[struct {
		History []historyOut `json:"history"`
	}](f.t, response, http.StatusOK).History
}

func (f *syncFixture) assertDownload(meta V2FileMeta, content string) {
	f.t.Helper()
	target := f.endpoint("download") + "?path=" + url.QueryEscape(meta.Path) + "&revision=" + strconv.FormatInt(meta.Revision, 10)
	response := f.request(http.MethodGet, target, f.token, "", nil)
	if response.Code != http.StatusOK || response.Body.String() != content {
		f.t.Fatalf("download = (%d, %q), want (200, %q)", response.Code, response.Body.String(), content)
	}
	sum := sha256.Sum256([]byte(content))
	wantHash := hex.EncodeToString(sum[:])
	if meta.Deleted || meta.Hash != wantHash || meta.Size != int64(len(content)) {
		f.t.Fatalf("download metadata = %+v, want hash %s and size %d", meta, wantHash, len(content))
	}
	if response.Header().Get("X-OSS-Revision") != strconv.FormatInt(meta.Revision, 10) || response.Header().Get("X-OSS-Hash") != wantHash {
		f.t.Fatalf("download revision/hash headers do not match metadata: %v", response.Header())
	}
	var file models.File
	if err := f.db.Where("vault_id = ? AND path = ?", f.vault.ID, meta.Path).First(&file).Error; err != nil {
		f.t.Fatalf("load stored file: %v", err)
	}
	stored, err := os.ReadFile(filepath.Join(f.cfg.Storage.DataDir, filepath.FromSlash(file.StorageKey)))
	if err != nil || string(stored) != content || file.Revision != meta.Revision || file.IsDeleted {
		f.t.Fatalf("stored file = %+v, body %q, error %v", file, stored, err)
	}
}
