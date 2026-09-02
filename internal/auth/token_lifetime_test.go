package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/jwt"
	"github.com/oss/oss-server/internal/models"
)

func TestTokenIssuersUseScopedLifetimes(t *testing.T) {
	cfg := &config.Config{Auth: config.AuthConfig{
		JWTSecret:          "test-secret",
		JWTTTLHours:        72,
		WebSessionTTLHours: 24,
		DeviceJWTTTLHours:  720,
	}}
	user := models.User{Username: "alice", Role: "user"}
	user.ID = 42

	_, genericTTL, err := IssueToken(cfg, user)
	if err != nil {
		t.Fatalf("issue generic token: %v", err)
	}
	_, webTTL, err := IssueWebToken(cfg, user)
	if err != nil {
		t.Fatalf("issue web token: %v", err)
	}
	deviceToken, deviceTTL, err := IssueDeviceToken(cfg, user, jwt.DeviceID("device-1"))
	if err != nil {
		t.Fatalf("issue device token: %v", err)
	}

	if genericTTL != int64((72*time.Hour)/time.Second) {
		t.Errorf("generic ttl = %d, want 72 hours", genericTTL)
	}
	if webTTL != int64((24*time.Hour)/time.Second) {
		t.Errorf("web ttl = %d, want 24 hours", webTTL)
	}
	if deviceTTL != int64((30*24*time.Hour)/time.Second) {
		t.Errorf("device ttl = %d, want 30 days", deviceTTL)
	}
	claims, err := jwt.Parse(cfg.Auth.JWTSecret, deviceToken)
	if err != nil {
		t.Fatalf("parse device token: %v", err)
	}
	if claims.ExpAt-claims.IssuedAt != int64((30*24*time.Hour)/time.Second) {
		t.Errorf("device claim lifetime = %d, want 30 days", claims.ExpAt-claims.IssuedAt)
	}
}

func TestMiddlewareReturnsStableCodeForExpiredToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	cfg := newTestConfig()
	user := models.User{Username: "alice", PasswordHash: "x", Role: "user"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	token, err := jwt.Sign(cfg.Auth.JWTSecret, jwt.Claims{UserID: user.ID}, -time.Hour)
	if err != nil {
		t.Fatalf("sign expired token: %v", err)
	}
	router := gin.New()
	router.Use(Middleware(db, cfg))
	router.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, req)

	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if recorder.Code != http.StatusUnauthorized || body.Code != "token_expired" {
		t.Fatalf("status=%d code=%q, want 401 token_expired", recorder.Code, body.Code)
	}
}
