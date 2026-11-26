package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/config"
	"github.com/helantianshen/oss-sync/internal/jwt"
	"github.com/helantianshen/oss-sync/internal/models"
)

func TestAuthenticationDistinguishesInvalidCredentialsAndUnavailableDatabase(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "auth.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.User{}); err != nil {
		t.Fatal(err)
	}
	user := models.User{Username: "tester", TokenVersion: 2}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Auth: config.AuthConfig{JWTSecret: "test-key"}}
	router := gin.New()
	router.GET("/", Middleware(db, cfg), func(c *gin.Context) { c.Status(http.StatusNoContent) })

	check := func(t *testing.T, token string, status int, code string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("status %d, want %d, body %s", rec.Code, status, rec.Body)
		}
		if code != "" {
			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != code {
				t.Fatalf("body %s, want code %q", rec.Body, code)
			}
		}
	}
	claims := jwt.Claims{UserID: user.ID, TokenVersion: user.TokenVersion}
	valid := jwt.MustSign(cfg.Auth.JWTSecret, claims, time.Hour)
	check(t, valid, http.StatusNoContent, "")
	check(t, "", http.StatusUnauthorized, "authorization_missing")
	check(t, "invalid", http.StatusUnauthorized, "token_invalid")
	check(t, jwt.MustSign(cfg.Auth.JWTSecret, claims, -time.Hour), http.StatusUnauthorized, "token_expired")
	check(t, jwt.MustSign(cfg.Auth.JWTSecret, jwt.Claims{UserID: user.ID}, time.Hour), http.StatusUnauthorized, "token_invalid")
	check(t, jwt.MustSign(cfg.Auth.JWTSecret, jwt.Claims{UserID: user.ID + 1}, time.Hour), http.StatusUnauthorized, "token_invalid")

	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	check(t, valid, http.StatusServiceUnavailable, "auth_unavailable")
}
