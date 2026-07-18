package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oss/oss-server/internal/jwt"
	"github.com/oss/oss-server/internal/models"
)

func TestAuthMiddleware_NoToken(t *testing.T) {
	srv, _, _ := newTestServer(t)
	code, _ := doJSON(t, srv.Router(), "POST", "/api/sync/check", "", map[string]any{
		"mode": "full", "files": []any{},
	})
	if code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", code)
	}
}

func TestRegisterRequiresAdminAfterFirst(t *testing.T) {
	srv, _, _ := newTestServer(t)
	router := srv.Router()
	registerAndLogin(t, router, "first", "password123")
	code, _ := doJSON(t, router, "POST", "/api/auth/register", "", map[string]string{
		"username": "second", "password": "password123",
	})
	if code != http.StatusUnauthorized {
		t.Errorf("second register without admin auth: expected 401, got %d", code)
	}
}

func TestAnonymousRegistrationEnabled(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.Cfg.Auth.AllowAnonymousRegistration = true
	router := srv.Router()
	registerAndLogin(t, router, "owner", "password123")

	code, body := doJSON(t, router, "POST", "/api/auth/register", "", map[string]string{
		"username": "member", "password": "password123",
	})
	if code != http.StatusOK || body["role"] != "user" {
		t.Errorf("anonymous register: status=%d body=%v", code, body)
	}
}

func TestAnonymousRegistrationCannotCreateAdmin(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.Cfg.Auth.AllowAnonymousRegistration = true
	router := srv.Router()
	registerAndLogin(t, router, "owner", "password123")

	code, body := doJSON(t, router, "POST", "/api/auth/register", "", map[string]string{
		"username": "member", "password": "password123", "role": "admin",
	})
	if code != http.StatusOK || body["role"] != "user" {
		t.Errorf("anonymous admin escalation: status=%d body=%v", code, body)
	}
}

func TestFirstRegistrationCreatesAdmin(t *testing.T) {
	srv, db, _ := newTestServer(t)
	code, body := doJSON(t, srv.Router(), "POST", "/api/auth/register", "", map[string]string{
		"username": "owner", "password": "password123", "role": "user",
	})
	if code != http.StatusOK || body["role"] != "admin" {
		t.Errorf("first register: status=%d body=%v", code, body)
	}
	var settingsCount int64
	db.Model(&models.UserSetting{}).Count(&settingsCount)
	if settingsCount != 1 {
		t.Errorf("default user settings count: got %d want 1", settingsCount)
	}
}

func TestLoginAcceptsCorrectPassword(t *testing.T) {
	srv, _, _ := newTestServer(t)
	router := srv.Router()
	registerAndLogin(t, router, "owner", "password123")
	code, body := doJSON(t, router, "POST", "/api/auth/login", "", map[string]string{
		"username": "owner", "password": "password123",
	})
	if code != http.StatusOK || body["token"] == "" || body["role"] != "admin" {
		t.Errorf("login: status=%d body=%v", code, body)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	srv, _, _ := newTestServer(t)
	router := srv.Router()
	registerAndLogin(t, router, "owner", "password123")
	code, _ := doJSON(t, router, "POST", "/api/auth/login", "", map[string]string{
		"username": "owner", "password": "wrong-password",
	})
	if code != http.StatusUnauthorized {
		t.Errorf("wrong password: got %d want 401", code)
	}
}

func TestRegisterRejectsInvalidCredentials(t *testing.T) {
	srv, _, _ := newTestServer(t)
	code, _ := doJSON(t, srv.Router(), "POST", "/api/auth/register", "", map[string]string{
		"username": "ab", "password": "short",
	})
	if code != http.StatusBadRequest {
		t.Errorf("invalid registration: got %d want 400", code)
	}
}

func TestAdminCanRegisterUser(t *testing.T) {
	srv, _, _ := newTestServer(t)
	router := srv.Router()
	adminToken := registerAndLogin(t, router, "owner", "password123")
	code, body := doJSON(t, router, "POST", "/api/auth/register", adminToken, map[string]string{
		"username": "member", "password": "password123",
	})
	if code != http.StatusOK || body["role"] != "user" {
		t.Errorf("admin register: status=%d body=%v", code, body)
	}
}

func TestAdminCanRegisterAdmin(t *testing.T) {
	srv, _, _ := newTestServer(t)
	router := srv.Router()
	adminToken := registerAndLogin(t, router, "owner", "password123")
	code, body := doJSON(t, router, "POST", "/api/auth/register", adminToken, map[string]string{
		"username": "second-admin", "password": "password123", "role": "admin",
	})
	if code != http.StatusOK || body["role"] != "admin" {
		t.Errorf("admin register admin: status=%d body=%v", code, body)
	}
}

func TestNonAdminCannotRegisterUser(t *testing.T) {
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	registerAndLogin(t, router, "owner", "password123")
	member := models.User{Username: "member", PasswordHash: "unused", Role: "user"}
	if err := db.Create(&member).Error; err != nil {
		t.Fatal(err)
	}
	memberToken := jwt.MustSign(srv.Cfg.Auth.JWTSecret, jwt.Claims{
		UserID: member.ID, Username: member.Username, Role: member.Role,
	}, time.Hour)

	code, _ := doJSON(t, router, "POST", "/api/auth/register", memberToken, map[string]string{
		"username": "blocked", "password": "password123",
	})
	if code != http.StatusForbidden {
		t.Errorf("non-admin register: got %d want 403", code)
	}
}

func TestAuthStatusTracksFirstAdmin(t *testing.T) {
	srv, _, _ := newTestServer(t)
	router := srv.Router()
	code, body := doJSON(t, router, "GET", "/api/auth/status", "", nil)
	if code != http.StatusOK ||
		body["needs_first_admin"] != true ||
		body["registration_mode"] != "first_admin" {
		t.Fatalf("status before register: %d %v", code, body)
	}
	registerAndLogin(t, router, "owner", "password123")
	code, body = doJSON(t, router, "GET", "/api/auth/status", "", nil)
	if code != http.StatusOK ||
		body["needs_first_admin"] != false ||
		body["registration_mode"] != "admin_only" {
		t.Fatalf("status after register: %d %v", code, body)
	}
}

func TestAuthStatusReportsAnonymousRegistration(t *testing.T) {
	srv, _, _ := newTestServer(t)
	router := srv.Router()
	registerAndLogin(t, router, "owner", "password123")
	srv.Cfg.Auth.AllowAnonymousRegistration = true

	code, body := doJSON(t, router, "GET", "/api/auth/status", "", nil)
	if code != http.StatusOK ||
		body["needs_first_admin"] != false ||
		body["registration_mode"] != "anonymous" {
		t.Fatalf("anonymous registration status: %d %v", code, body)
	}
}

func TestBasicAuthRejectsWrongPassword(t *testing.T) {
	srv, _, _ := newTestServer(t)
	router := srv.Router()
	registerAndLogin(t, router, "owner", "password123")
	req := httptest.NewRequest("POST", "/api/sync/check", strings.NewReader(`{"mode":"full","files":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("owner", "wrong-password")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong basic password: got %d want 401", w.Code)
	}
}
