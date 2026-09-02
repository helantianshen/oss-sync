package webui

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/jwt"
	"github.com/oss/oss-server/internal/models"
)

func TestSetSessionCookieUsesWebLifetime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Auth: config.AuthConfig{
		JWTSecret:          "test-secret",
		JWTTTLHours:        72,
		WebSessionTTLHours: 24,
	}}
	handler := &Handler{Cfg: cfg}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "https://sync.example.com/login", nil)
	user := &models.User{Username: "alice", Role: "user"}
	user.ID = 42

	handler.setSessionCookie(ctx, user)

	wantMaxAge := int((24 * time.Hour) / time.Second)
	var sessionValue string
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == sessionCookie {
			sessionValue = cookie.Value
		}
		if cookie.MaxAge != wantMaxAge {
			t.Errorf("cookie %s max-age = %d, want %d", cookie.Name, cookie.MaxAge, wantMaxAge)
		}
	}
	if sessionValue == "" {
		t.Fatal("session cookie was not set")
	}
	claims, err := jwt.Parse(cfg.Auth.JWTSecret, sessionValue)
	if err != nil {
		t.Fatalf("parse session token: %v", err)
	}
	if claims.ExpAt-claims.IssuedAt != int64(wantMaxAge) {
		t.Errorf("session claim lifetime = %d, want %d", claims.ExpAt-claims.IssuedAt, wantMaxAge)
	}
}
