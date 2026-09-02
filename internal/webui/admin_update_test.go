package webui

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/oss/oss-server/internal/auth"
	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/models"
	"github.com/oss/oss-server/internal/update"
	"github.com/oss/oss-server/internal/version"
)

func newWebUITestDB(t *testing.T) (*gorm.DB, *config.Config, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dataDir := t.TempDir()
	db, err := gorm.Open(sqlite.Open(filepath.Join(dataDir, "test.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := db.AutoMigrate(&models.User{}, &models.UserSetting{}, &models.SystemSetting{}, &models.Vault{}, &models.VaultSetting{}, &models.StorageIssue{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg := &config.Config{
		Server:   config.ServerConfig{Host: "127.0.0.1", Port: 8080, Mode: gin.TestMode, MaxFileSizeMB: 100},
		Storage:  config.StorageConfig{DataDir: dataDir},
		Database: config.DatabaseConfig{Driver: "sqlite", DSN: filepath.Join(dataDir, "test.db")},
		Auth:     config.AuthConfig{JWTSecret: "test-secret-32-bytes-long-xxxxxx", JWTTTLHours: 1},
		Update:   config.UpdateConfig{GitHubRepo: "fake/oss-sync"},
	}
	// ensure jwt secret in db
	_ = auth.EnsureDatabaseJWTSecret(db, cfg)
	return db, cfg, dataDir
}

func createTestUserWithHash(t *testing.T, db *gorm.DB, username, role string) *models.User {
	t.Helper()
	u, err := auth.CreateAccount(db, username, "pass12345", role)
	if err != nil {
		t.Fatalf("create account %s: %v", username, err)
	}
	return u
}

func issueWebSession(t *testing.T, cfg *config.Config, user *models.User) (sessionCookie *http.Cookie, csrfToken string) {
	t.Helper()
	token, _, err := auth.IssueWebToken(cfg, *user)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	csrfToken = "test-csrf-token-123"
	sessionCookie = &http.Cookie{Name: sessionCookieName(), Value: token, Path: "/"}
	return sessionCookie, csrfToken
}

func sessionCookieName() string { return "oss_web_session" }
func csrfCookieName() string    { return "oss_csrf" }

func newWebUIHandlerWithUpdate(t *testing.T, db *gorm.DB, cfg *config.Config) (*Handler, *update.Manager, *update.Service) {
	t.Helper()
	h, err := New(db, cfg)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	// setup updater and manager
	dataDir := cfg.Storage.DataDir
	exePath := filepath.Join(t.TempDir(), "oss-server")
	_ = os.WriteFile(exePath, []byte("old-binary"), 0o755)
	mgr, err := update.NewManager(dataDir)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	up, err := update.NewUpdater(cfg, update.Options{ExecPath: exePath, Verifier: func(string, string) error { return nil }})
	if err != nil {
		t.Fatalf("new updater: %v", err)
	}
	// mock helper launch/verify for trigger tests
	origLaunch := updateLaunchHelperFn()
	update.SetLaunchHelperFn(func(string, string) error { return nil })
	t.Cleanup(func() { update.SetLaunchHelperFn(origLaunch) })
	origVerify := updateVerifyStagedFn()
	update.SetVerifyStagedFileFn(func(string, string, string) error { return nil })
	t.Cleanup(func() { update.SetVerifyStagedFileFn(origVerify) })
	svc := update.NewService(mgr, up, cfg)
	h.SetUpdateService(svc, up)
	return h, mgr, svc
}

func updateLaunchHelperFn() func(string, string) error         { return nil }
func updateVerifyStagedFn() func(string, string, string) error { return nil }

// NOTE: we patch via update package exported setters directly in tests above.

// helper to perform request with session+csrf
func doWebRequest(t *testing.T, h *Handler, method, path string, form url.Values, session *http.Cookie, csrf string, withCSRFHeader bool) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.Register(r)
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if session != nil {
		req.AddCookie(session)
	}
	if csrf != "" {
		req.AddCookie(&http.Cookie{Name: csrfCookieName(), Value: csrf})
		if withCSRFHeader {
			req.Header.Set("X-CSRF-Token", csrf)
		} else if form != nil {
			// form already contains _csrf if needed; if not, header fallback is only path
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAdminUpdate_RequiresLogin(t *testing.T) {
	db, cfg, _ := newWebUITestDB(t)
	h, _, _ := newWebUIHandlerWithUpdate(t, db, cfg)
	w := doWebRequest(t, h, "POST", "/dashboard/admin/system/update/check", url.Values{"_csrf": {"test-csrf-token-123"}}, nil, "test-csrf-token-123", true)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		// requireSession redirects to /login for unauthenticated
		t.Fatalf("want redirect to login, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "/login") {
		t.Errorf("redirect location %q should contain /login", loc)
	}
}

func TestAdminUpdate_NonAdminForbidden(t *testing.T) {
	db, cfg, _ := newWebUITestDB(t)
	h, _, _ := newWebUIHandlerWithUpdate(t, db, cfg)
	user := createTestUserWithHash(t, db, "bob", "user")
	sess, csrf := issueWebSession(t, cfg, user)
	w := doWebRequest(t, h, "POST", "/dashboard/admin/system/update/check", url.Values{"_csrf": {csrf}}, sess, csrf, false)
	// non-admin is redirected to /dashboard by requireAdmin
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("non-admin should be redirected, got %d body %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "/dashboard") {
		t.Errorf("non-admin redirect %q should go to /dashboard", loc)
	}
}

func TestAdminUpdate_MissingCSRF(t *testing.T) {
	db, cfg, _ := newWebUITestDB(t)
	h, _, _ := newWebUIHandlerWithUpdate(t, db, cfg)
	user := createTestUserWithHash(t, db, "admin1", "admin")
	sess, csrf := issueWebSession(t, cfg, user)
	// send POST without CSRF header/form value (but cookie present)
	form := url.Values{}
	w := doWebRequest(t, h, "POST", "/dashboard/admin/system/update/check", form, sess, csrf, false)
	// validCSRF requires X-CSRF-Token or _csrf form; missing should be 403
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF should be 403, got %d body %s", w.Code, w.Body.String())
	}
}

func TestAdminUpdate_ForgedCheckID(t *testing.T) {
	db, cfg, _ := newWebUITestDB(t)
	h, _, _ := newWebUIHandlerWithUpdate(t, db, cfg)
	user := createTestUserWithHash(t, db, "admin2", "admin")
	sess, csrf := issueWebSession(t, cfg, user)
	form := url.Values{"_csrf": {csrf}, "check_id": {"nonexistent-id"}, "expected_version": {"9.9.9"}, "confirm": {"on"}}
	w := doWebRequest(t, h, "POST", "/dashboard/admin/system/update", form, sess, csrf, true)
	// when CSRF via header, form still needs _csrf but requireSession checks cookie==header; we also send form _csrf via body reader? Our doWebRequest sends form.Encode() includes _csrf
	// now trigger should return 404 or 400 for forged check
	if w.Code == http.StatusAccepted {
		t.Fatalf("forged check should not succeed, got 202")
	}
	body := w.Body.String()
	if !strings.Contains(body, "check") && w.Code != http.StatusNotFound && w.Code != http.StatusBadGateway {
		t.Errorf("forged check response %d body %s should indicate check error", w.Code, body)
	}
}

func TestAdminUpdate_VersionMismatch(t *testing.T) {
	origVer := version.Version
	version.Version = "1.0.0"
	t.Cleanup(func() { version.Version = origVer })
	db, cfg, _ := newWebUITestDB(t)
	h, mgr, _ := newWebUIHandlerWithUpdate(t, db, cfg)
	// issue a checked candidate 9.9.9
	checkID := newCheckedForWebUITest(t, mgr, "9.9.9")
	user := createTestUserWithHash(t, db, "admin3", "admin")
	sess, csrf := issueWebSession(t, cfg, user)
	form := url.Values{"_csrf": {csrf}, "check_id": {checkID}, "expected_version": {"9.9.10"}, "confirm": {"on"}}
	w := doWebRequest(t, h, "POST", "/dashboard/admin/system/update", form, sess, csrf, true)
	if w.Code != http.StatusConflict {
		t.Fatalf("mismatched version should be 409, got %d body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "check_mismatch") {
		t.Errorf("body should contain check_mismatch, got %s", w.Body.String())
	}
}

func TestAdminUpdate_SuccessfulCheckAndTrigger(t *testing.T) {
	origVer := version.Version
	version.Version = "1.0.0"
	t.Cleanup(func() { version.Version = origVer })
	db, cfg, dataDir := newWebUITestDB(t)
	// mock GitHub release server
	assetName, _ := update.AssetName("9.9.9", runtime.GOOS, runtime.GOARCH)
	content := []byte("fake-binary-content")
	var serveContent []byte
	l := strings.ToLower(assetName)
	if strings.HasSuffix(l, ".tar.gz") {
		serveContent = makeTarGzWebUI(t, map[string][]byte{"oss-server": content})
	} else if strings.HasSuffix(l, ".zip") {
		serveContent = makeZipWebUI(t, map[string][]byte{"oss-server": content})
	} else {
		serveContent = content
	}
	digest := digestOfBytesWebUI(serveContent)
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/releases/latest") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":1001,"tag_name":"v9.9.9","html_url":"https://example.com/releases/tag/v9.9.9","draft":false,"prerelease":false,"assets":[{"id":2001,"name":"` + assetName + `","browser_download_url":"` + `https://example.com/` + assetName + `","url":"","size":` + strconv.Itoa(len(serveContent)) + `,"digest":"` + digest + `"}]}`))
			return
		}
		if strings.Contains(r.URL.Path, assetName) {
			w.Header().Set("Content-Length", strconv.Itoa(len(serveContent)))
			_, _ = w.Write(serveContent)
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(ghSrv.Close)
	exePath := filepath.Join(t.TempDir(), "oss-server")
	_ = os.WriteFile(exePath, []byte("old-binary"), 0o755)
	mgr, _ := update.NewManager(dataDir)
	cfg.Storage.DataDir = dataDir
	up, _ := update.NewUpdater(cfg, update.Options{ExecPath: exePath, APIBase: ghSrv.URL, HTTPClient: ghSrv.Client(), Verifier: func(string, string) error { return nil }})
	// mock launch/verify for trigger
	origLaunch := getLaunchFn()
	update.SetLaunchHelperFn(func(string, string) error { return nil })
	t.Cleanup(func() { update.SetLaunchHelperFn(origLaunch) })
	origVerify := getVerifyFn()
	update.SetVerifyStagedFileFn(func(string, string, string) error { return nil })
	t.Cleanup(func() { update.SetVerifyStagedFileFn(origVerify) })
	// start asset server for download (Service.StartHelperUpdate downloads from AssetURL which points to example.com; need to rewrite to ghSrv)
	// Our candidate IssueChecked in Service.Check uses BrowserDownloadURL = https://example.com/... but Service.StartHelperUpdate will try to download that URL and fail.
	// To avoid download failure, we instead bypass Service.Check and create checked candidate directly pointing to ghSrv.
	// So we patch: create manager checked candidate pointing to ghSrv
	h, err := New(db, cfg)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	svc := update.NewService(mgr, up, cfg)
	h.SetUpdateService(svc, up)
	// Create checked candidate pointing to ghSrv for trigger
	assetURL := ghSrv.URL + "/" + assetName
	cand, _ := update.NewCandidate("v9.9.9", runtime.GOOS, runtime.GOARCH, assetURL, "https://example.com/releases/tag/v9.9.9", int64(len(serveContent)), 1001, 2001, digest)
	cc, _ := mgr.IssueChecked(*cand, time.Hour)
	checkID := cc.ID
	user := createTestUserWithHash(t, db, "admin4", "admin")
	sess, csrf := issueWebSession(t, cfg, user)
	_ = svc // avoid unused
	// now trigger with correct check_id+version+confirm should return 202
	form := url.Values{"_csrf": {csrf}, "check_id": {checkID}, "expected_version": {"9.9.9"}, "confirm": {"on"}}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.Register(r)
	body := strings.NewReader(form.Encode())
	req := httptest.NewRequest("POST", "/dashboard/admin/system/update", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(sess)
	req.AddCookie(&http.Cookie{Name: csrfCookieName(), Value: csrf})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("trigger should be 202, got %d body %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["ok"] != true {
		t.Errorf("ok should be true, got %v", resp["ok"])
	}
	op, ok := resp["operation"].(map[string]any)
	if !ok || op["id"] == "" {
		t.Errorf("operation missing %v", resp["operation"])
	}

	// status endpoint should reflect active operation
	req2 := httptest.NewRequest("GET", "/dashboard/admin/system/update/status", nil)
	req2.AddCookie(sess)
	req2.AddCookie(&http.Cookie{Name: csrfCookieName(), Value: csrf})
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("status should be 200, got %d", w2.Code)
	}
	var status adminUpdateStatus
	if err := json.Unmarshal(w2.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !status.IsUpdating {
		t.Errorf("status IsUpdating should be true while operation active")
	}
	if status.CurrentVersion != "1.0.0" {
		t.Errorf("current version %q want 1.0.0", status.CurrentVersion)
	}
}

func TestAdminUpdate_MissingConfirm(t *testing.T) {
	db, cfg, _ := newWebUITestDB(t)
	h, mgr, _ := newWebUIHandlerWithUpdate(t, db, cfg)
	checkID := newCheckedForWebUITest(t, mgr, "9.9.9")
	user := createTestUserWithHash(t, db, "admin5", "admin")
	sess, csrf := issueWebSession(t, cfg, user)
	form := url.Values{"_csrf": {csrf}, "check_id": {checkID}, "expected_version": {"9.9.9"}}
	w := doWebRequest(t, h, "POST", "/dashboard/admin/system/update", form, sess, csrf, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing confirm should be 400, got %d body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "confirm") {
		t.Errorf("body should mention confirm, got %s", w.Body.String())
	}
}

func TestAdminUpdateCheck_usesRequestedSource(t *testing.T) {
	origVer := version.Version
	version.Version = "1.0.0"
	t.Cleanup(func() { version.Version = origVer })

	var requestPath string
	content := fakeExecBytesWebUI()
	assetName, err := update.AssetName("v9.9.9", runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatalf("asset name: %v", err)
	}
	digest := digestOfBytesWebUI(content)
	var upstream *httptest.Server
	upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w,
			`{"id":1001,"tag_name":"v9.9.9","html_url":%q,"draft":false,"prerelease":false,"assets":[{"id":2001,"name":%q,"browser_download_url":%q,"size":%d,"digest":%q}]}`,
			upstream.URL+"/releases/v9.9.9", assetName, upstream.URL+"/assets/"+assetName, len(content), digest)
	}))
	t.Cleanup(upstream.Close)

	db, cfg, dataDir := newWebUITestDB(t)
	exePath := filepath.Join(t.TempDir(), "oss-server")
	if err := os.WriteFile(exePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	mgr, err := update.NewManager(dataDir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	updater, err := update.NewUpdater(cfg, update.Options{
		ExecPath:   exePath,
		APIBase:    upstream.URL,
		HTTPClient: upstream.Client(),
		Verifier:   func(string, string) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewUpdater: %v", err)
	}
	svc := update.NewService(mgr, updater, cfg)
	h, err := New(db, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.SetUpdateService(svc, updater)
	admin := createTestUserWithHash(t, db, "source-admin", "admin")
	session, csrf := issueWebSession(t, cfg, admin)
	w := doWebRequest(t, h, "POST", "/dashboard/admin/system/update/check", url.Values{
		"_csrf":           {csrf},
		"download_source": {"custom"},
		"download_proxy":  {upstream.URL + "/mirror"},
	}, session, csrf, true)
	if w.Code != http.StatusOK {
		t.Fatalf("check status %d body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(requestPath, "/mirror/https://") {
		t.Fatalf("release API did not use requested source: %q", requestPath)
	}
	if !strings.Contains(w.Body.String(), `"check_id"`) {
		t.Fatalf("check response missing check_id: %s", w.Body.String())
	}
}

func TestAdminSystemTemplate_UpdatePanel(t *testing.T) {
	tpl, err := template.New("web").Funcs(template.FuncMap{
		"formatBytes": formatBytes,
		"timeFmt":     func(v time.Time) string { return v.Format("2006-01-02 15:04") },
	}).ParseFS(webFS, "templates/admin_system.html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	data := struct {
		Layout layoutData
		Data   map[string]any
	}{
		Layout: layoutData{CSRF: "csrf-token", Language: "zh"},
		Data: map[string]any{
			"RegistrationEnabled":   true,
			"SyncMode":              "user_choice",
			"DefaultRecycleBinDays": 30,
			"MaxLongPollWaitSec":    20,
			"MaxSyncDebounceSec":    60,
			"MaxRecycleBinDays":     90,
			"MaxVaultStorageMB":     int64(10240),
			"MaxUploadSizeMB":       int64(50),
			"ConfigMaxUploadSizeMB": int64(100),
			"DatabaseDriver":        "sqlite",
			"DatabaseDSN":           "data/oss.db",
			"Update": adminUpdateStatus{
				CurrentVersion: "1.0.0",
				Env:            "dev",
				GOOS:           runtime.GOOS,
				GOARCH:         runtime.GOARCH,
				CapabilityOK:   true,
				DownloadSource: "proxy",
				IsUpdating:     false,
			},
		},
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "admin-system", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	page := buf.String()
	for _, needle := range []string{
		`data-update-panel`,
		`data-update-check-form`,
		`data-update-trigger-form`,
		`data-update-status`,
		`name="check_id"`,
		`name="expected_version"`,
		`data-update-action="/dashboard/admin/system/update"`,
		`data-update-check-btn`,
		`data-update-trigger-btn`,
		`data-update-trigger-form data-update-action="/dashboard/admin/system/update" hidden`,
		`data-capability-ready="true"`,
		`data-external-update="false"`,
		`data-msg-checking="正在检查新版本，请稍候…"`,
		`data-update-download-source`,
		`value="proxy" selected`,
		`value="official"`,
		`value="custom"`,
		`data-update-custom-proxy hidden`,
		`aria-live="polite"`,
	} {
		if !strings.Contains(page, needle) {
			t.Errorf("update panel missing %q", needle)
		}
	}
	// when not updating, buttons should not be disabled
	if strings.Contains(page, `data-update-check-btn" disabled`) {
		t.Errorf("check button should not be disabled when idle")
	}
	if strings.Contains(page, `<form method="post" action="/dashboard/admin/system/update`) {
		t.Error("update controls must not submit forms or navigate away from the page")
	}
	if !strings.Contains(page, `type="button" data-update-check-btn`) {
		t.Error("update check must be a page-local button")
	}
	if strings.Contains(page, `<script`) || strings.Contains(page, `style=`) {
		t.Error("update panel must not rely on inline script or style blocked by CSP")
	}
	// Container deployments use the same in-process update path; the writable
	// runtime directory lets Docker restart the updated process.
	data.Data["Update"] = adminUpdateStatus{
		CurrentVersion: "1.0.0",
		Env:            "prod",
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
		CapabilityOK:   true,
		DownloadSource: "proxy",
	}
	buf.Reset()
	if err := tpl.ExecuteTemplate(&buf, "admin-system", data); err != nil {
		t.Fatalf("render container deployment: %v", err)
	}
	page2 := buf.String()
	if !strings.Contains(page2, `data-capability-ready="true"`) || !strings.Contains(page2, `data-update-download-source`) {
		t.Errorf("container deployment should expose the in-process update controls")
	}
	if strings.Contains(page2, `data-external-update="true"`) {
		t.Errorf("container deployment should not require an external updater")
	}
}

func TestAdminUpdateStatus_ContainerUsesInProcessUpdater(t *testing.T) {
	db, cfg, _ := newWebUITestDB(t)
	h, _, _ := newWebUIHandlerWithUpdate(t, db, cfg)
	t.Setenv("OSS_DEPLOYMENT_MODE", "container")

	status := h.buildUpdateStatus()
	if !status.CapabilityOK || status.ExternalUpdate || status.CapabilityErr != "" {
		t.Fatalf("unexpected container update status: %+v", status)
	}
}

// helpers

func newCheckedForWebUITest(t *testing.T, mgr *update.Manager, ver string) string {
	t.Helper()
	assetName, _ := update.AssetName(ver, runtime.GOOS, runtime.GOARCH)
	content := []byte("fake-binary")
	l := strings.ToLower(assetName)
	var serveContent []byte
	if strings.HasSuffix(l, ".tar.gz") {
		serveContent = makeTarGzWebUI(t, map[string][]byte{"oss-server": content})
	} else if strings.HasSuffix(l, ".zip") {
		serveContent = makeZipWebUI(t, map[string][]byte{"oss-server": content})
	} else {
		serveContent = content
	}
	digest := digestOfBytesWebUI(serveContent)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(serveContent)))
		_, _ = w.Write(serveContent)
	}))
	t.Cleanup(srv.Close)
	assetURL := srv.URL + "/" + assetName
	cand, err := update.NewCandidate("v"+ver, runtime.GOOS, runtime.GOARCH, assetURL, "https://example.com/releases/tag/v"+ver, int64(len(serveContent)), 1001, 2001, digest)
	if err != nil {
		t.Fatalf("NewCandidate: %v", err)
	}
	cc, err := mgr.IssueChecked(*cand, time.Hour)
	if err != nil {
		t.Fatalf("IssueChecked: %v", err)
	}
	return cc.ID
}

func fakeExecBytesWebUI() []byte {
	var hdr []byte
	switch runtime.GOOS {
	case "windows":
		hdr = []byte{'M', 'Z', 0, 0}
	case "linux":
		hdr = []byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 0}
	case "darwin":
		hdr = []byte{0xcf, 0xfa, 0xed, 0xfe}
	default:
		hdr = []byte{'M', 'Z', 0, 0}
	}
	return append(hdr, []byte(" fake exec 9.9.9 ")...)
}

func makeTarGzWebUI(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if len(data) == 0 {
			data = fakeExecBytesWebUI()
		}
		if !hasValidMagic(data) {
			data = append(fakeExecBytesWebUI(), data...)
		}
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func makeZipWebUI(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		if len(data) == 0 {
			data = fakeExecBytesWebUI()
		}
		if !hasValidMagic(data) {
			data = append(fakeExecBytesWebUI(), data...)
		}
		f, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := f.Write(data); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	_ = zw.Close()
	return buf.Bytes()
}

func hasValidMagic(b []byte) bool {
	if len(b) < 2 {
		return false
	}
	switch runtime.GOOS {
	case "windows":
		return b[0] == 'M' && b[1] == 'Z'
	case "linux":
		return len(b) >= 4 && b[0] == 0x7f && b[1] == 'E' && b[2] == 'L' && b[3] == 'F'
	case "darwin":
		return len(b) >= 4 && ((b[0] == 0xcf && b[1] == 0xfa) || (b[0] == 0xfe && b[1] == 0xed) || (b[0] == 0xca && b[1] == 0xfe))
	}
	return b[0] == 'M' && b[1] == 'Z'
}
func digestOfBytesWebUI(b []byte) string {
	// compute real sha256:<64 hex>
	// local import to avoid top-level cycle
	//nolint:revive
	h := sha256Sum(b)
	return "sha256:" + h
}

func sha256Sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func getLaunchFn() func(string, string) error { return func(string, string) error { return nil } }
func getVerifyFn() func(string, string, string) error {
	return func(string, string, string) error { return nil }
}
