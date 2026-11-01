package syncapi

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/helantianshen/oss-sync/internal/auth"
	"github.com/helantianshen/oss-sync/internal/config"
	"github.com/helantianshen/oss-sync/internal/database"
	"github.com/helantianshen/oss-sync/internal/deviceauth"
	"github.com/helantianshen/oss-sync/internal/jwt"
	"github.com/helantianshen/oss-sync/internal/models"
)

func TestSQLiteConcurrentDownloadsRecordDeviceActivity(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{Database: config.DatabaseConfig{Driver: "sqlite", DSN: filepath.Join(root, "oss.db")}, Storage: config.StorageConfig{DataDir: filepath.Join(root, "storage")}}
	db, err := database.Init(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	t.Cleanup(func() { _ = pool.Close() })
	db = db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	if err := database.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	user := models.User{Username: "concurrency-test", PasswordHash: "unused"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	vault := models.Vault{ID: "test-vault", OwnerID: user.ID, Name: "test"}
	if err := db.Create(&vault).Error; err != nil {
		t.Fatal(err)
	}
	device := models.ClientDevice{UserID: user.ID, ClientID: "device", Status: deviceauth.DeviceStatusApproved}
	if err := db.Create(&device).Error; err != nil {
		t.Fatal(err)
	}
	if err := deviceauth.GrantAccess(db, user.ID, "device", vault.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	body := []byte("# Concurrent download\n完整内容测试\n")
	hash := fmt.Sprintf("%x", sha256.Sum256(body))
	file := models.File{UserID: user.ID, VaultID: vault.ID, Path: "notes/test.md", Type: "markdown", Hash: hash, Size: int64(len(body)), MTime: 1, Revision: 1}
	if err := db.Create(&file).Error; err != nil {
		t.Fatal(err)
	}
	handler := New(db, cfg)
	diskPath := handler.fileDiskPath(file)
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.GET("/api/vaults/:vault_id/sync/download", func(c *gin.Context) {
		c.Set(auth.ContextKeyCurrentUser, &user)
		c.Set(auth.ContextKeyIdentity, &auth.Identity{User: &user, DeviceID: jwt.DeviceID("device"), HasDID: true})
		handler.V2Download(c)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	client := server.Client()
	const workers, rounds = 6, 30
	start := make(chan struct{})
	failures := make(chan error, workers)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			for round := 0; round < rounds; round++ {
				request, err := http.NewRequest(http.MethodGet, server.URL+"/api/vaults/test-vault/sync/download?path=notes/test.md&revision=1", nil)
				if err != nil {
					failures <- err
					return
				}
				request.Header.Set(deviceauth.ClientIDHeader, "device")
				response, err := client.Do(request)
				if err != nil {
					failures <- err
					return
				}
				content, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if err != nil {
					failures <- err
					return
				}
				if response.StatusCode != http.StatusOK || !bytes.Equal(content, body) || response.Header.Get("X-OSS-Hash") != hash {
					failures <- fmt.Errorf("download failed: HTTP %d: %q", response.StatusCode, content)
					return
				}
			}
		}()
	}
	close(start)
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	var binding models.DeviceVault
	if err := db.First(&binding).Error; err != nil {
		t.Fatal(err)
	}
	if binding.LastCursor != 0 {
		t.Fatalf("a download acknowledged an unread cursor: %d", binding.LastCursor)
	}
	var after models.File
	if err := db.First(&after, file.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Revision != 1 || after.Hash != hash {
		t.Fatal("download changed file metadata")
	}
	t.Logf("%d concurrent-download requests passed with exact content and device activity writes", workers*rounds)
}
