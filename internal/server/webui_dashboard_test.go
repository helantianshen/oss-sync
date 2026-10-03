package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/helantianshen/oss-sync/internal/auth"
	"github.com/helantianshen/oss-sync/internal/deviceauth"
	"github.com/helantianshen/oss-sync/internal/models"
	"gorm.io/gorm"
)

func setCustomFragmentsEnabledForTest(t *testing.T, db *gorm.DB, enabled bool) {
	t.Helper()

	var setting models.SystemSetting
	err := db.First(&setting, 1).Error
	if err == nil {
		if err := db.Model(&setting).Update("custom_fragments_enabled", enabled).Error; err != nil {
			t.Fatalf("set custom fragments policy: %v", err)
		}
		return
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("lookup system setting: %v", err)
	}

	setting = models.SystemSetting{ID: 1, CustomFragmentsEnabled: enabled}
	if err := db.Create(&setting).Error; err != nil {
		t.Fatalf("create system setting: %v", err)
	}
}

// webLogin 通过统一登录入口建立会话，返回会话与 CSRF cookie
func webLogin(t *testing.T, router *gin.Engine, user, pass string) (*http.Cookie, *http.Cookie) {
	t.Helper()
	res := doForm(t, router, http.MethodPost, "/login", url.Values{
		"username": {user},
		"password": {pass},
	}, nil)
	if res.Code != http.StatusSeeOther || res.Header().Get("Location") != "/dashboard" {
		t.Fatalf("web login: status=%d location=%q body=%s", res.Code, res.Header().Get("Location"), res.Body)
	}
	session, csrf := webCookies(t, res)
	if session == nil || csrf == nil {
		t.Fatal("login did not set session and csrf cookies")
	}
	return session, csrf
}

// uploadViaV1 用 v1 协议上传文件（默认仓库，无需设备授权）供控制台展示
func uploadViaV1(t *testing.T, router *gin.Engine, token, path, content string) {
	t.Helper()
	code, body := uploadFile(t, router, token, path, content, 1700000000000)
	if code != http.StatusOK {
		t.Fatalf("v1 upload %s: %d %v", path, code, body)
	}
}

func TestWebConsoleLoginLogoutAndDashboard(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	if _, err := auth.CreateAccount(db, "console-user", "password123", "user"); err != nil {
		t.Fatal(err)
	}

	// 未登录访问 dashboard 重定向到登录页
	anon := doForm(t, router, http.MethodGet, "/dashboard", nil, nil)
	if anon.Code != http.StatusSeeOther || anon.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous dashboard: %d %q", anon.Code, anon.Header().Get("Location"))
	}

	loginPage := doForm(t, router, http.MethodGet, "/login", nil, nil)
	if loginPage.Code != http.StatusOK ||
		!strings.Contains(loginPage.Body.String(), "登录你的同步账本") ||
		!strings.Contains(loginPage.Body.String(), "/ui/assets/theme.js") {
		t.Fatalf("login page: %d body=%s", loginPage.Code, loginPage.Body)
	}

	session, csrf := webLogin(t, router, "console-user", "password123")

	dashboard := doForm(t, router, http.MethodGet, "/dashboard", nil, session, csrf)
	if dashboard.Code != http.StatusOK {
		t.Fatalf("dashboard: %d body=%s", dashboard.Code, dashboard.Body)
	}
	body := dashboard.Body.String()
	// 侧边栏元素：用户功能分组、SVG 图标、二级菜单、退出、主题切换
	for _, want := range []string{"OSS Sync", "用户设置", "仓库管理", "首页", "个人中心", "设备管理", "退出登录", "跟随系统", "app.js", `data-nav-icon="overview"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	// 用户名旁显示普通用户角色徽章
	if !strings.Contains(body, "role--user") {
		t.Fatalf("dashboard missing user role badge")
	}
	// 普通用户不能看到管理员设置菜单
	if strings.Contains(body, "管理员设置") {
		t.Fatalf("regular user sees admin menu")
	}

	// 登出会清除会话 cookie（JWT 无状态，客户端不再持有）
	logout := doForm(t, router, http.MethodPost, "/logout", url.Values{}, session, csrf)
	if logout.Code != http.StatusSeeOther || logout.Header().Get("Location") != "/login" {
		t.Fatalf("logout: %d %q", logout.Code, logout.Header().Get("Location"))
	}
	cleared := false
	for _, cookie := range logout.Result().Cookies() {
		if cookie.Name == "oss_web_session" && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not clear session cookie")
	}
}

func TestWebConsoleRequiresCSRFForStateChanges(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	if _, err := auth.CreateAccount(db, "csrf-user", "password123", "user"); err != nil {
		t.Fatal(err)
	}
	session, csrf := webLogin(t, router, "csrf-user", "password123")

	// 无 CSRF 的 POST 返回 403
	noCSRF := doFormRaw(t, router, http.MethodPost, "/dashboard/account/password", url.Values{
		"old_password": {"password123"}, "new_password": {"newpass123"}, "new_password_confirm": {"newpass123"},
	}, session, nil)
	if noCSRF.Code != http.StatusForbidden {
		t.Fatalf("POST without csrf: %d", noCSRF.Code)
	}

	// 带 CSRF 的 POST 成功
	withCSRF := doForm(t, router, http.MethodPost, "/dashboard/account/password", url.Values{
		"old_password": {"password123"}, "new_password": {"newpass123"}, "new_password_confirm": {"newpass123"},
	}, session, csrf)
	if withCSRF.Code != http.StatusSeeOther {
		t.Fatalf("change password with csrf: %d body=%s", withCSRF.Code, withCSRF.Body)
	}
	// 旧密码失效、新密码生效
	if _, err := auth.AuthenticateCredentials(db, "csrf-user", "password123"); err == nil {
		t.Fatal("old password still works")
	}
	if _, err := auth.AuthenticateCredentials(db, "csrf-user", "newpass123"); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
}

func TestWebConsoleVaultSettingsCustomFragmentsAreBoundedOnSave(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	setCustomFragmentsEnabledForTest(t, db, true)

	apiToken := registerAndLogin(t, router, "vault-owner", "password123")
	session, csrf := webLogin(t, router, "vault-owner", "password123")
	vaultID := defaultVaultIDFromAPI(t, router, apiToken)

	longHeader := strings.Repeat("H", 2500)
	longFooter := strings.Repeat("F", 2500)
	res := doForm(t, router, http.MethodPost, "/dashboard/vaults/"+vaultID+"/settings", url.Values{
		"theme_name":       {"default"},
		"recycle_bin_days": {"0"},
		"is_public_blog":   {"on"},
		"custom_header":    {longHeader},
		"custom_footer":    {longFooter},
	}, session, csrf)
	if res.Code != http.StatusSeeOther || !strings.Contains(res.Header().Get("Location"), "saved=1") {
		t.Fatalf("save vault settings: %d %q", res.Code, res.Header().Get("Location"))
	}

	var setting models.VaultSetting
	if err := db.Where("vault_id = ?", vaultID).First(&setting).Error; err != nil {
		t.Fatal(err)
	}
	if len([]rune(setting.CustomHeader)) != 2000 {
		t.Fatalf("custom_header length=%d, want 2000", len([]rune(setting.CustomHeader)))
	}
	if setting.CustomHeader != strings.Repeat("H", 2000) {
		t.Fatalf("custom_header not truncated to first runes")
	}
	if len([]rune(setting.CustomFooter)) != 2000 {
		t.Fatalf("custom_footer length=%d, want 2000", len([]rune(setting.CustomFooter)))
	}
	if setting.CustomFooter != strings.Repeat("F", 2000) {
		t.Fatalf("custom_footer not truncated to first runes")
	}
}

func TestWebConsoleVaultSettingsHidesCustomFragmentsFromParticipants(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	ownerToken := registerAndLogin(t, router, "fragment-owner", "password123")
	vaultID := defaultVaultIDFromAPI(t, router, ownerToken)
	code, _ := doJSON(t, router, http.MethodPost, "/api/auth/register", "", map[string]string{
		"username": "fragment-participant", "password": "password123",
	})
	if code != http.StatusOK {
		t.Fatalf("register participant: %d", code)
	}
	code, _ = doJSON(t, router, http.MethodPost, "/api/vaults/"+vaultID+"/members", ownerToken, map[string]string{
		"username": "fragment-participant", "role": "participant",
	})
	if code != http.StatusNoContent {
		t.Fatalf("add participant: %d", code)
	}
	participantSession, participantCSRF := webLogin(t, router, "fragment-participant", "password123")

	page := doForm(t, router, http.MethodGet, "/dashboard/vaults/"+vaultID+"/settings", nil, participantSession, participantCSRF)
	response := doForm(t, router, http.MethodPost, "/dashboard/vaults/"+vaultID+"/settings", url.Values{
		"theme_name": {"default"}, "custom_header": {"participant must not save this"},
	}, participantSession, participantCSRF)

	if page.Code != http.StatusOK || strings.Contains(page.Body.String(), `name="custom_header"`) {
		t.Fatalf("participant settings page exposed custom fragments: status=%d body=%s", page.Code, page.Body)
	}
	if response.Code != http.StatusSeeOther || !strings.Contains(response.Header().Get("Location"), "error=") {
		t.Fatalf("participant custom fragment save was not rejected: %d %q", response.Code, response.Header().Get("Location"))
	}
	var setting models.VaultSetting
	if err := db.Where("vault_id = ?", vaultID).First(&setting).Error; err != nil {
		t.Fatal(err)
	}
	if setting.CustomHeader != "" || setting.CustomFooter != "" {
		t.Fatalf("participant changed custom fragments: %#v", setting)
	}
}

func TestWebConsoleVaultSettingsForbidCustomFragmentsWhenPolicyDisabled(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	setCustomFragmentsEnabledForTest(t, db, true)
	setCustomFragmentsEnabledForTest(t, db, false)

	ownerToken := registerAndLogin(t, router, "policy-owner", "password123")
	vaultID := defaultVaultIDFromAPI(t, router, ownerToken)
	session, csrf := webLogin(t, router, "policy-owner", "password123")
	setCustomFragmentsEnabledForTest(t, db, true)
	seed := doForm(t, router, http.MethodPost, "/dashboard/vaults/"+vaultID+"/settings", url.Values{
		"theme_name":       {"default"},
		"recycle_bin_days": {"0"},
		"is_public_blog":   {"on"},
		"custom_header":    {"ORIGINAL HEADER"},
		"custom_footer":    {"ORIGINAL FOOTER"},
	}, session, csrf)
	if seed.Code != http.StatusSeeOther || !strings.Contains(seed.Header().Get("Location"), "saved=1") {
		t.Fatalf("seed custom fragments when enabled: %d %q", seed.Code, seed.Header().Get("Location"))
	}
	setCustomFragmentsEnabledForTest(t, db, false)

	res := doForm(t, router, http.MethodPost, "/dashboard/vaults/"+vaultID+"/settings", url.Values{
		"theme_name":       {"default"},
		"recycle_bin_days": {"0"},
		"is_public_blog":   {"on"},
		"custom_header":    {"FORGED HEADER"},
		"custom_footer":    {"FORGED FOOTER"},
	}, session, csrf)

	if res.Code != http.StatusSeeOther || !strings.Contains(res.Header().Get("Location"), "saved=1") {
		t.Fatalf("save settings with disabled policy should still succeed: %d %q", res.Code, res.Header().Get("Location"))
	}
	var setting models.VaultSetting
	if err := db.Where("vault_id = ?", vaultID).First(&setting).Error; err != nil {
		t.Fatal(err)
	}
	if setting.CustomHeader != "ORIGINAL HEADER" || setting.CustomFooter != "ORIGINAL FOOTER" {
		t.Fatalf("forged custom fragments should be preserved, got %q / %q", setting.CustomHeader, setting.CustomFooter)
	}

	settingsPage := doForm(t, router, http.MethodGet, "/dashboard/vaults/"+vaultID+"/settings", nil, session, csrf)
	if settingsPage.Code != http.StatusOK ||
		strings.Contains(settingsPage.Body.String(), `name="custom_header"`) ||
		strings.Contains(settingsPage.Body.String(), `name="custom_footer"`) {
		t.Fatalf("settings page should not expose custom fragments when disabled: %d body=%s", settingsPage.Code, settingsPage.Body)
	}
}

func TestWebConsoleVaultSettingsForbidForgedCustomFragmentsFromParticipantsWhenPolicyDisabled(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()

	setCustomFragmentsEnabledForTest(t, db, true)
	ownerToken := registerAndLogin(t, router, "forged-participant-owner", "password123")
	vaultID := defaultVaultIDFromAPI(t, router, ownerToken)

	// 策略启用期间先写入历史片段
	ownerSession, ownerCSRF := webLogin(t, router, "forged-participant-owner", "password123")
	seed := doForm(t, router, http.MethodPost, "/dashboard/vaults/"+vaultID+"/settings", url.Values{
		"theme_name":       {"default"},
		"recycle_bin_days": {"0"},
		"is_public_blog":   {"on"},
		"custom_header":    {"ORIGINAL FORGED OWNER HEADER"},
		"custom_footer":    {"ORIGINAL FORGED OWNER FOOTER"},
	}, ownerSession, ownerCSRF)
	if seed.Code != http.StatusSeeOther || !strings.Contains(seed.Header().Get("Location"), "saved=1") {
		t.Fatalf("seed settings with enabled policy: %d %q", seed.Code, seed.Header().Get("Location"))
	}
	setCustomFragmentsEnabledForTest(t, db, false)

	// 加一个参与者成员
	code, _ := doJSON(t, router, http.MethodPost, "/api/auth/register", "", map[string]string{
		"username": "forged-participant", "password": "password123",
	})
	if code != http.StatusOK {
		t.Fatalf("register participant: %d", code)
	}
	code, _ = doJSON(t, router, http.MethodPost, "/api/vaults/"+vaultID+"/members", ownerToken,
		map[string]string{"username": "forged-participant", "role": "participant"})
	if code != http.StatusNoContent {
		t.Fatalf("add participant: %d", code)
	}
	participantSession, participantCSRF := webLogin(t, router, "forged-participant", "password123")

	// 参与者在策略关闭时伪造自定义片段
	res := doForm(t, router, http.MethodPost, "/dashboard/vaults/"+vaultID+"/settings", url.Values{
		"theme_name":       {"default"},
		"recycle_bin_days": {"0"},
		"is_public_blog":   {"on"},
		"custom_header":    {"FORGED PARTICIPANT HEADER"},
		"custom_footer":    {"FORGED PARTICIPANT FOOTER"},
	}, participantSession, participantCSRF)
	if res.Code != http.StatusSeeOther || !strings.Contains(res.Header().Get("Location"), "error=") {
		t.Fatalf("participant forged payload should be rejected: %d %q", res.Code, res.Header().Get("Location"))
	}

	// 已存历史片段应保持不变
	var setting models.VaultSetting
	if err := db.Where("vault_id = ?", vaultID).First(&setting).Error; err != nil {
		t.Fatal(err)
	}
	if setting.CustomHeader != "ORIGINAL FORGED OWNER HEADER" || setting.CustomFooter != "ORIGINAL FORGED OWNER FOOTER" {
		t.Fatalf("forged participant payload modified custom fragments: %#v", setting)
	}
}

func TestWebConsoleVaultFilesDeleteAndRecycleRestore(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, db, dataDir := newTestServer(t)
	router := srv.Router()

	ownerToken := registerAndLogin(t, router, "files-owner", "password123")
	vaultID := defaultVaultIDFromAPI(t, router, ownerToken)
	// 为 files-user 办参与者授权
	code, memberLogin := doJSON(t, router, http.MethodPost, "/api/auth/register", "",
		map[string]string{"username": "files-user", "password": "password123"})
	if code != http.StatusOK {
		t.Fatalf("register files-user: %d %v", code, memberLogin)
	}
	var memberUser models.User
	if err := db.Where("username = ?", "files-user").First(&memberUser).Error; err != nil {
		t.Fatal(err)
	}
	code, _ = doJSON(t, router, http.MethodPost, "/api/vaults/"+vaultID+"/members", ownerToken,
		map[string]string{"username": "files-user", "role": "manager"})
	if code != http.StatusNoContent {
		t.Fatalf("add member: %d", code)
	}
	uploadViaV1(t, router, ownerToken, "Notes/A.md", "# Hello\n\n- [x] Finished\n- [ ] Next")
	uploadViaV1(t, router, ownerToken, "pic.png", "png-bytes")
	uploadViaV1(t, router, ownerToken, "Rendered.md", "# Preview\n\n![pic](pic.png)\n<script>alert('x')</script>")

	session, csrf := webLogin(t, router, "files-user", "password123")
	var ownerUser models.User
	if err := db.Where("username = ?", "files-owner").First(&ownerUser).Error; err != nil {
		t.Fatal(err)
	}

	// 根目录只展示直接文件和可进入的目录
	page := doForm(t, router, http.MethodGet, "/dashboard/vaults/"+vaultID, nil, session, csrf)
	if page.Code != http.StatusOK ||
		!strings.Contains(page.Body.String(), ">Notes</a>") ||
		!strings.Contains(page.Body.String(), "?dir=Notes") ||
		!strings.Contains(page.Body.String(), "pic.png") ||
		!strings.Contains(page.Body.String(), `side-nav__vault-name`) {
		t.Fatalf("vault files page: %d body=%s", page.Code, page.Body)
	}

	// 进入目录后显示直接子文件、面包屑和没有双重编码的预览链接
	folderPage := doForm(t, router, http.MethodGet, "/dashboard/vaults/"+vaultID+"?dir=Notes", nil, session, csrf)
	if folderPage.Code != http.StatusOK ||
		!strings.Contains(folderPage.Body.String(), "根目录") ||
		!strings.Contains(folderPage.Body.String(), ">Notes</span>") ||
		!strings.Contains(folderPage.Body.String(), "A.md") ||
		strings.Contains(folderPage.Body.String(), "%252F") {
		t.Fatalf("vault folder page: %d body=%s", folderPage.Code, folderPage.Body)
	}

	// 预览文本文件
	preview := doForm(t, router, http.MethodGet,
		"/dashboard/vaults/"+vaultID+"/files/download?path=Notes%2FA.md", nil, session, csrf)
	if preview.Code != http.StatusOK || preview.Body.String() != "# Hello\n\n- [x] Finished\n- [ ] Next" {
		t.Fatalf("preview: %d body=%q", preview.Code, preview.Body.String())
	}

	// Markdown 预览页渲染 HTML，不直接返回原始 Markdown 文本
	markdownPreview := doForm(t, router, http.MethodGet,
		"/dashboard/vaults/"+vaultID+"/files/preview?path=Notes%2FA.md", nil, session, csrf)
	if markdownPreview.Code != http.StatusOK ||
		!strings.Contains(markdownPreview.Body.String(), `<article class="markdown-preview">`) ||
		!strings.Contains(markdownPreview.Body.String(), `class="file-breadcrumbs"`) ||
		!strings.Contains(markdownPreview.Body.String(), `?dir=Notes`) ||
		!strings.Contains(markdownPreview.Body.String(), `<h1>A.md</h1>`) ||
		!strings.Contains(markdownPreview.Body.String(), `aria-current="page">A.md</span>`) ||
		!strings.Contains(markdownPreview.Body.String(), `type="checkbox"`) ||
		!strings.Contains(markdownPreview.Body.String(), "<h1>Hello</h1>") {
		t.Fatalf("markdown preview: %d body=%s", markdownPreview.Code, markdownPreview.Body)
	}

	renderedWithAsset := doForm(t, router, http.MethodGet,
		"/dashboard/vaults/"+vaultID+"/files/preview?path=Rendered.md&mode=rendered", nil, session, csrf)
	if renderedWithAsset.Code != http.StatusOK ||
		!strings.Contains(renderedWithAsset.Body.String(), `<article class="markdown-preview">`) ||
		!strings.Contains(renderedWithAsset.Body.String(), `aria-selected="true">`) {
		t.Fatalf("markdown preview rendered: %d body=%s", renderedWithAsset.Code, renderedWithAsset.Body)
	}

	var assetShare models.Share
	if err := db.Where("user_id = ? AND vault_id = ? AND target_path = ? AND is_folder = ?",
		ownerUser.ID, vaultID, "Rendered.md", false).First(&assetShare).Error; err != nil {
		t.Fatalf("preview share row: %v", err)
	}

	sharedAsset := doForm(t, router, http.MethodGet,
		"/assets/"+assetShare.ShareID+"?ref=pic.png", nil, nil)
	if sharedAsset.Code != http.StatusOK {
		t.Fatalf("shared asset: %d body=%q", sharedAsset.Code, sharedAsset.Body.String())
	}
	if !strings.Contains(renderedWithAsset.Body.String(), "/assets/"+assetShare.ShareID+"?ref=pic.png") {
		t.Fatalf("rendered preview asset url missing: %s", renderedWithAsset.Body.String())
	}
	if sharedAsset.Body.String() != "png-bytes" {
		t.Fatalf("shared asset content: %q", sharedAsset.Body.String())
	}
	if got := sharedAsset.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("shared asset nosniff: %q", got)
	}

	markdownSource := doForm(t, router, http.MethodGet,
		"/dashboard/vaults/"+vaultID+"/files/preview?path=Rendered.md&mode=source", nil, session, csrf)
	if markdownSource.Code != http.StatusOK ||
		!strings.Contains(markdownSource.Body.String(), `<article class="markdown-preview"><pre><code>`) ||
		!strings.Contains(markdownSource.Body.String(), `&lt;script&gt;`) {
		t.Fatalf("markdown source preview: %d body=%s", markdownSource.Code, markdownSource.Body)
	}

	// 删除文件（写入历史 + 移入回收站）
	del := doForm(t, router, http.MethodPost, "/dashboard/vaults/"+vaultID+"/files/delete",
		url.Values{"path": {"Notes/A.md"}}, session, csrf)
	if del.Code != http.StatusSeeOther {
		t.Fatalf("web delete file: %d body=%s", del.Code, del.Body)
	}
	var tomb models.File
	if err := db.Where("vault_id = ? AND path = ?", vaultID, "Notes/A.md").First(&tomb).Error; err != nil {
		t.Fatal(err)
	}
	if !tomb.IsDeleted {
		t.Fatal("file not marked deleted")
	}
	// 正文应在回收站目录
	if !strings.Contains(tomb.StorageKey, "recycle") {
		t.Fatalf("storage key not recycled: %s", tomb.StorageKey)
	}
	// 历史记录含 delete 动作与设备名“网页控制台”
	var hist models.FileHistory
	if err := db.Where("vault_id = ? AND file_path = ? AND action = ?", vaultID, "Notes/A.md", "delete").
		Order("id desc").First(&hist).Error; err != nil {
		t.Fatal(err)
	}
	if hist.DeviceName != "网页控制台" || hist.Username != "files-user" {
		t.Fatalf("history actor: device=%q user=%q", hist.DeviceName, hist.Username)
	}

	// 文件预览和仓库记录均可进入该文件的中文修改记录与详情
	historyPage := doForm(t, router, http.MethodGet,
		"/dashboard/vaults/"+vaultID+"/history?path=Notes%2FA.md", nil, session, csrf)
	if historyPage.Code != http.StatusOK ||
		!strings.Contains(historyPage.Body.String(), "Notes/A.md") ||
		!strings.Contains(historyPage.Body.String(), ">删除</span>") ||
		!strings.Contains(historyPage.Body.String(), "/history/"+strconv.FormatUint(uint64(hist.ID), 10)) {
		t.Fatalf("file history page: %d body=%s", historyPage.Code, historyPage.Body)
	}

	historyDetail := doForm(t, router, http.MethodGet,
		"/dashboard/vaults/"+vaultID+"/history/"+strconv.FormatUint(uint64(hist.ID), 10), nil, session, csrf)
	if historyDetail.Code != http.StatusOK ||
		!strings.Contains(historyDetail.Body.String(), "变更内容") ||
		!strings.Contains(historyDetail.Body.String(), "恢复此版本") {
		t.Fatalf("file history detail: %d body=%s", historyDetail.Code, historyDetail.Body)
	}

	// 回收站列出该文件
	recycle := doForm(t, router, http.MethodGet, "/dashboard/vaults/"+vaultID+"/recycle", nil, session, csrf)
	if recycle.Code != http.StatusOK || !strings.Contains(recycle.Body.String(), "Notes/A.md") {
		t.Fatalf("recycle page: %d body=%s", recycle.Code, recycle.Body)
	}

	// 恢复
	restore := doForm(t, router, http.MethodPost,
		"/dashboard/vaults/"+vaultID+"/recycle/"+strconv.FormatUint(uint64(tomb.ID), 10)+"/restore",
		nil, session, csrf)
	if restore.Code != http.StatusSeeOther {
		t.Fatalf("restore: %d body=%s", restore.Code, restore.Body)
	}
	var restored models.File
	if err := db.Where("vault_id = ? AND path = ?", vaultID, "Notes/A.md").First(&restored).Error; err != nil {
		t.Fatal(err)
	}
	if restored.IsDeleted {
		t.Fatal("file still deleted after restore")
	}
	content, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(restored.StorageKey)))
	if err != nil || string(content) != "# Hello\n\n- [x] Finished\n- [ ] Next" {
		t.Fatalf("restored content: %q err=%v", content, err)
	}
	// 校验 restore 历史记录
	var restoreHist models.FileHistory
	if err := db.Where("vault_id = ? AND file_path = ? AND action = ?", vaultID, "Notes/A.md", "restore").
		Order("id desc").First(&restoreHist).Error; err != nil {
		t.Fatal(err)
	}

	// 分享创建、切换 allow_copy 与取消
	share := doForm(t, router, http.MethodPost, "/dashboard/vaults/"+vaultID+"/shares",
		url.Values{"target_path": {"Notes/A.md"}}, session, csrf)
	if share.Code != http.StatusSeeOther {
		t.Fatalf("create share: %d body=%s", share.Code, share.Body)
	}
	var shareRow models.Share
	if err := db.Where("vault_id = ? AND target_path = ?", vaultID, "Notes/A.md").First(&shareRow).Error; err != nil {
		t.Fatal(err)
	}
	toggle := doForm(t, router, http.MethodPost,
		"/dashboard/vaults/"+vaultID+"/shares/"+shareRow.ShareID+"/allow_copy",
		url.Values{"allow_copy": {"true"}}, session, csrf)
	if toggle.Code != http.StatusSeeOther {
		t.Fatalf("toggle allow_copy: %d", toggle.Code)
	}
	if err := db.Where("share_id = ?", shareRow.ShareID).First(&shareRow).Error; err != nil || !shareRow.AllowCopy {
		t.Fatalf("allow_copy not set: %#v err=%v", shareRow, err)
	}
	revoke := doForm(t, router, http.MethodPost,
		"/dashboard/vaults/"+vaultID+"/shares/"+shareRow.ShareID+"/delete", nil, session, csrf)
	if revoke.Code != http.StatusSeeOther {
		t.Fatalf("revoke share: %d", revoke.Code)
	}
	if err := db.Where("share_id = ?", shareRow.ShareID).First(&models.Share{}).Error; err == nil {
		t.Fatal("share remains after revoke")
	}
}

func TestWebConsoleDevicesAndAdminPages(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	if _, err := auth.CreateAccount(db, "root", "root-password-123", "admin"); err != nil {
		t.Fatal(err)
	}
	token := registerAndLogin(t, router, "owner-dev", "password123")
	vaultID := defaultVaultIDFromAPI(t, router, token)

	// 通过带设备头的登录请求登记设备（pending）
	reqBody := fmt.Sprintf(`{"username":"owner-dev","password":"password123"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(deviceauth.ClientIDHeader, "desktop-1")
	req.Header.Set(deviceauth.DeviceNameHeader, "桌面机")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("device login: %d %s", w.Code, w.Body.String())
	}
	var owner models.User
	if err := db.Where("username = ?", "owner-dev").First(&owner).Error; err != nil {
		t.Fatal(err)
	}
	var pending models.ClientDevice
	if err := db.Where("user_id = ? AND client_id = ?", owner.ID, "desktop-1").First(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if pending.Status != "pending" {
		t.Fatalf("device status = %q, want pending", pending.Status)
	}

	session, csrf := webLogin(t, router, "owner-dev", "password123")
	devPage := doForm(t, router, http.MethodGet, "/dashboard/devices", nil, session, csrf)
	if devPage.Code != http.StatusOK || !strings.Contains(devPage.Body.String(), "桌面机") {
		t.Fatalf("devices page: %d body=%s", devPage.Code, devPage.Body)
	}
	if !strings.Contains(devPage.Body.String(), `class="device-auth-summary"`) ||
		!strings.Contains(devPage.Body.String(), `data-modal-open="`) ||
		!strings.Contains(devPage.Body.String(), `class="modal device-auth-modal"`) {
		t.Fatalf("devices page must render compact authorization summary: %s", devPage.Body)
	}
	// 批准设备
	approve := doForm(t, router, http.MethodPost, "/dashboard/devices/desktop-1/approve", nil, session, csrf)
	if approve.Code != http.StatusSeeOther {
		t.Fatalf("approve device: %d", approve.Code)
	}
	if err := db.Where("client_id = ?", "desktop-1").First(&pending).Error; err != nil || pending.Status != "approved" {
		t.Fatalf("device not approved: %#v err=%v", pending, err)
	}
	// 授权仓库
	authz := doForm(t, router, http.MethodPost, "/dashboard/devices/desktop-1/authorize",
		url.Values{"vault_ids": {vaultID}}, session, csrf)
	if authz.Code != http.StatusSeeOther {
		t.Fatalf("authorize vault: %d", authz.Code)
	}
	var access models.DeviceVaultAccess
	if err := db.Where("client_id = ? AND vault_id = ?", "desktop-1", vaultID).First(&access).Error; err != nil {
		t.Fatal(err)
	}

	// 管理员：用户管理页
	rootSession, rootCSRF := webLogin(t, router, "root", "root-password-123")
	users := doForm(t, router, http.MethodGet, "/dashboard/admin", nil, rootSession, rootCSRF)
	if users.Code != http.StatusOK ||
		!strings.Contains(users.Body.String(), "用户管理") ||
		!strings.Contains(users.Body.String(), "owner-dev") ||
		!strings.Contains(users.Body.String(), "root") {
		t.Fatalf("admin users: %d body=%s", users.Code, users.Body)
	}
	// 管理员同时拥有普通用户功能和管理员专属设置
	if !strings.Contains(users.Body.String(), "用户设置") ||
		!strings.Contains(users.Body.String(), "管理员设置") {
		t.Fatal("user/admin navigation groups missing for admin")
	}
	// 管理员用户名旁显示管理员角色徽章
	if !strings.Contains(users.Body.String(), "role--admin") {
		t.Fatal("admin role badge missing for admin")
	}
	// 全部仓库页
	allVaults := doForm(t, router, http.MethodGet, "/dashboard/admin/vaults", nil, rootSession, rootCSRF)
	if allVaults.Code != http.StatusOK || !strings.Contains(allVaults.Body.String(), vaultID) {
		t.Fatalf("admin vaults: %d body=%s", allVaults.Code, allVaults.Body)
	}
	// 全部设备页
	allDevices := doForm(t, router, http.MethodGet, "/dashboard/admin/devices", nil, rootSession, rootCSRF)
	allDevicesBody := allDevices.Body.String()
	if allDevices.Code != http.StatusOK || !strings.Contains(allDevicesBody, "desktop-1") {
		t.Fatalf("admin devices: %d body=%s", allDevices.Code, allDevices.Body)
	}
	if !strings.Contains(allDevicesBody, "<th>操作</th>") ||
		!strings.Contains(allDevicesBody, `class="device-auth-summary"`) ||
		!strings.Contains(allDevicesBody, `class="device-auth-summary__actions"`) ||
		!strings.Contains(allDevicesBody, `data-modal-open="device-modal-`) ||
		!strings.Contains(allDevicesBody, `class="modal device-auth-modal"`) ||
		!strings.Contains(allDevicesBody, `form="device-revoke-`) {
		t.Fatalf("admin device authorization must use a summary, rightmost actions, and modal: %s", allDevices.Body)
	}
	// 系统设置页
	system := doForm(t, router, http.MethodGet, "/dashboard/admin/system", nil, rootSession, rootCSRF)
	if system.Code != http.StatusOK || !strings.Contains(system.Body.String(), "默认回收站保留天数") {
		t.Fatalf("admin system: %d body=%s", system.Code, system.Body)
	}

	// 管理员重置 owner-dev 密码
	reset := doForm(t, router, http.MethodPost,
		"/dashboard/admin/users/"+strconv.FormatUint(uint64(owner.ID), 10)+"/reset-password",
		url.Values{"new_password": {"new-pw-123"}, "new_password_confirm": {"new-pw-123"}}, rootSession, rootCSRF)
	if reset.Code != http.StatusSeeOther {
		t.Fatalf("admin reset password: %d", reset.Code)
	}
	if _, err := auth.AuthenticateCredentials(db, "owner-dev", "new-pw-123"); err != nil {
		t.Fatalf("reset password failed: %v", err)
	}
}

func TestWebConsoleVaultSettingsAllowOwnerToSelectTheme(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	if _, err := auth.CreateAccount(db, "theme-root", "root-password-123", "admin"); err != nil {
		t.Fatal(err)
	}
	ownerToken := registerAndLogin(t, router, "theme-owner", "password123")
	vaultID := defaultVaultIDFromAPI(t, router, ownerToken)
	session, csrf := webLogin(t, router, "theme-owner", "password123")

	page := doForm(t, router, http.MethodGet, "/dashboard/vaults/"+vaultID+"/settings", nil, session, csrf)
	if page.Code != http.StatusOK ||
		!strings.Contains(page.Body.String(), `select name="theme_name"`) ||
		!strings.Contains(page.Body.String(), `option value="default"`) ||
		!strings.Contains(page.Body.String(), `option value="papertrail"`) {
		t.Fatalf("vault theme selector missing: status=%d body=%s", page.Code, page.Body)
	}
	if strings.Contains(page.Body.String(), `name="sync_mode"`) || strings.Contains(page.Body.String(), "同步模式") {
		t.Fatalf("vault settings expose global sync policy: %s", page.Body)
	}

	saved := doForm(t, router, http.MethodPost, "/dashboard/vaults/"+vaultID+"/settings", url.Values{
		"theme_name":       {"papertrail"},
		"sync_mode":        {"long_poll"},
		"recycle_bin_days": {"30"},
	}, session, csrf)
	if saved.Code != http.StatusSeeOther {
		t.Fatalf("save vault theme: status=%d body=%s", saved.Code, saved.Body)
	}
	var setting models.VaultSetting
	if err := db.Where("vault_id = ?", vaultID).First(&setting).Error; err != nil {
		t.Fatal(err)
	}
	if setting.ThemeName != "papertrail" {
		t.Fatalf("theme name = %q, want papertrail", setting.ThemeName)
	}
	var system models.SystemSetting
	if err := db.First(&system, 1).Error; err != nil {
		t.Fatal(err)
	}
	if system.SyncMode != "user_choice" {
		t.Fatalf("ordinary owner forced global sync mode to %q", system.SyncMode)
	}
}

func TestWebConsolePluginSettingsKeepsSubmittedValuesWhenURLIsInvalid(t *testing.T) {
	// 仓库启用 Papertrail 插件，用户填全各类插件设置
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	ownerToken := registerAndLogin(t, router, "theme-settings-owner", "password123")
	vaultID := defaultVaultIDFromAPI(t, router, ownerToken)
	if err := db.Model(&models.VaultSetting{}).Where("vault_id = ?", vaultID).
		Updates(map[string]any{"theme_name": "papertrail", "theme_config": models.JSONMap{}}).Error; err != nil {
		t.Fatal(err)
	}
	session, csrf := webLogin(t, router, "theme-settings-owner", "password123")

	// URL 校验拒绝提交的 Logo URL
	response := doForm(t, router, http.MethodPost, "/dashboard/plugins/papertrail-settings/settings", url.Values{
		"vault_id":               {vaultID},
		"setting_blog_name":      {"Draft blog"},
		"setting_description":    {"Draft description"},
		"setting_logo_url":       {"javascript:alert(1)"},
		"group_buttons_label":    {"Home"},
		"group_buttons_url":      {"/"},
		"group_buttons_icon_url": {"/icon.svg"},
	}, session, csrf)

	// 插件错误页保留全部提交值，不回退到空的已存数据
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid theme settings status=%d, want 400; body=%s", response.Code, response.Body)
	}
	for _, want := range []string{"Draft blog", "Draft description", "javascript:alert(1)", "/icon.svg", "设置 博客 Logo URL 必须是 http(s) 或站内相对 URL"} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("invalid theme settings lost %q: body=%s", want, response.Body)
		}
	}
}

func TestWebConsolePersistsAccountLanguagePreference(t *testing.T) {
	// 已登录网页控制台用户保持中文默认偏好
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	registerAndLogin(t, router, "language-owner", "password123")
	session, csrf := webLogin(t, router, "language-owner", "password123")

	// 用户在账号设置中选择英文
	response := doForm(t, router, http.MethodPost, "/dashboard/account/language", url.Values{
		"web_language": {"en"},
	}, session, csrf)

	// 账号偏好持久化，并决定下一次页面渲染
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/dashboard/account?settings_saved=1#language" {
		t.Fatalf("save language: status=%d location=%q", response.Code, response.Header().Get("Location"))
	}
	var setting models.UserSetting
	if err := db.Where("user_id = ?", 1).First(&setting).Error; err != nil {
		t.Fatal(err)
	}
	if setting.WebLanguage != "en" {
		t.Fatalf("web language=%q, want en", setting.WebLanguage)
	}
	request := httptest.NewRequest(http.MethodGet, "/dashboard/account", nil)
	request.AddCookie(session)
	page := httptest.NewRecorder()
	router.ServeHTTP(page, request)
	if page.Code != http.StatusOK ||
		!strings.Contains(page.Body.String(), `<html lang="en"`) ||
		!strings.Contains(page.Body.String(), "Web language") ||
		strings.Contains(page.Body.String(), ">网页语言<") {
		t.Fatalf("account page: status=%d body=%s", page.Code, page.Body.String())
	}
}

func TestShareAPIAllowsVaultManagerToUpdateAllowCopy(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	if _, err := auth.CreateAccount(db, "share-root", "root-password-123", "admin"); err != nil {
		t.Fatal(err)
	}
	token := registerAndLogin(t, router, "share-owner", "password123")
	vaultID := defaultVaultIDFromAPI(t, router, token)
	uploadViaV1(t, router, token, "Shared.md", "# Shared")
	code, created := doJSON(t, router, http.MethodPost, "/api/shares", token, map[string]any{
		"vault_id": vaultID, "target_path": "Shared.md", "allow_copy": true,
	})
	if code != http.StatusOK {
		t.Fatalf("create share: status=%d body=%v", code, created)
	}
	shareID := created["share_id"].(string)

	code, updated := doJSON(t, router, http.MethodPatch, "/api/shares/"+shareID, token, map[string]any{
		"allow_copy": false,
	})
	if code != http.StatusOK || updated["allow_copy"] != false {
		t.Fatalf("update share: status=%d body=%v", code, updated)
	}
	var share models.Share
	if err := db.Where("share_id = ?", shareID).First(&share).Error; err != nil {
		t.Fatal(err)
	}
	if share.AllowCopy {
		t.Fatal("allow_copy remained enabled")
	}
}

func TestWebConsoleThemeAssetsAndOldAdminRedirects(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	if _, err := auth.CreateAccount(db, "console-user", "password123", "user"); err != nil {
		t.Fatal(err)
	}

	theme := doForm(t, router, http.MethodGet, "/ui/assets/theme.js", nil, nil)
	if theme.Code != http.StatusOK || !strings.Contains(theme.Body.String(), "OSSTheme") {
		t.Fatalf("theme.js: %d body=%s", theme.Code, theme.Body)
	}
	app := doForm(t, router, http.MethodGet, "/ui/assets/app.js", nil, nil)
	if app.Code != http.StatusOK || !strings.Contains(app.Body.String(), "initSidebar") {
		t.Fatalf("app.js: %d", app.Code)
	}
	css := doForm(t, router, http.MethodGet, "/ui/assets/console.css", nil, nil)
	if css.Code != http.StatusOK || !strings.Contains(css.Body.String(), ".sidebar") ||
		!strings.Contains(css.Body.String(), "data-theme") {
		t.Fatalf("console.css: %d", css.Code)
	}

	// 旧 /admin 入口重定向兼容
	legacy := doForm(t, router, http.MethodGet, "/admin", nil, nil)
	if legacy.Code != http.StatusMovedPermanently || legacy.Header().Get("Location") != "/dashboard/admin" {
		t.Fatalf("legacy /admin: %d %q", legacy.Code, legacy.Header().Get("Location"))
	}
	legacyLogin := doForm(t, router, http.MethodGet, "/admin/login", nil, nil)
	if legacyLogin.Code != http.StatusMovedPermanently || legacyLogin.Header().Get("Location") != "/login" {
		t.Fatalf("legacy /admin/login: %d %q", legacyLogin.Code, legacyLogin.Header().Get("Location"))
	}

	// 根路径是公开博客目录（暂无公开 Vault 时显示安全空状态）
	root := doForm(t, router, http.MethodGet, "/", nil, nil)
	if root.Code != http.StatusOK || !strings.Contains(root.Body.String(), "暂无公开博客") {
		t.Fatalf("root home: %d body=%s", root.Code, root.Body)
	}
	session, csrf := webLogin(t, router, "console-user", "password123")
	_ = csrf
	_ = session
}

func TestAuthPagesThemeControlsAndAuthLayout(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, _, _ := newTestServer(t)
	router := srv.Router()

	// 登录页：主题键、主题切换控件、theme.js 与 app.js 初始化脚本
	login := doForm(t, router, http.MethodGet, "/login", nil, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("login page: %d", login.Code)
	}
	for _, want := range []string{
		`data-theme-key="oss-console-theme"`,
		`id="theme-switcher"`,
		`data-theme-pref="auto"`,
		`data-theme-pref="light"`,
		`data-theme-pref="dark"`,
		`data-language-toggle`,
		`data-language-target="en"`,
		"English",
		`<script data-cfasync="false" src="/ui/assets/theme.js`,
		"/ui/assets/app.js",
		"console.css?v=",
		`data-cfasync="false"`,
	} {
		if !strings.Contains(login.Body.String(), want) {
			t.Errorf("login page missing %q", want)
		}
	}
	// 首帧主题启动脚本与 theme.js 必须退出 CDN 脚本代理（如 Cloudflare Rocket Loader），
	// 否则内联 nonce 脚本被改写为延迟类型，首帧仍按亮色绘制后才切暗色
	if got := strings.Count(login.Body.String(), `data-cfasync="false"`); got < 2 {
		t.Fatalf("login page has %d Rocket Loader opt-outs, want at least 2", got)
	}
	body := login.Body.String()
	styleMarker := `<style nonce="`
	styleStart := strings.Index(body, styleMarker)
	if styleStart < 0 {
		t.Fatal("login page missing nonce-protected critical theme style")
	}
	styleStart += len(styleMarker)
	styleEnd := strings.Index(body[styleStart:], `"`)
	if styleEnd < 1 {
		t.Fatal("login page critical theme style has an empty nonce")
	}
	nonce := body[styleStart : styleStart+styleEnd]
	if !strings.Contains(body, `html[data-theme="dark"]`) {
		t.Fatal("login page critical theme style missing dark background rule")
	}
	policy := login.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "style-src 'self' 'nonce-"+nonce+"'") {
		t.Fatalf("CSP does not authorize critical theme style nonce %q: %s", nonce, policy)
	}

	// 注册页同样带主题控件
	register := doForm(t, router, http.MethodGet, "/register", nil, nil)
	if register.Code != http.StatusOK {
		t.Fatalf("register page: %d", register.Code)
	}
	for _, want := range []string{`id="theme-switcher"`, `data-theme-pref="auto"`, `data-language-toggle`, "/ui/assets/app.js"} {
		if !strings.Contains(register.Body.String(), want) {
			t.Errorf("register page missing %q", want)
		}
	}

	// 公开博客目录：控制台主题键与主题控件
	home := doForm(t, router, http.MethodGet, "/", nil, nil)
	if home.Code != http.StatusOK {
		t.Fatalf("home page: %d", home.Code)
	}
	for _, want := range []string{
		`data-theme-key="oss-console-theme"`,
		`id="theme-switcher"`,
		"/ui/assets/app.js",
	} {
		if !strings.Contains(home.Body.String(), want) {
			t.Errorf("home page missing %q", want)
		}
	}

	// 控制台 CSS：认证页单列布局 + 暗色安全 wordmark 变量 + 主题与语言按钮单排 + 禁用浏览器缓存
	css := doForm(t, router, http.MethodGet, "/ui/assets/console.css", nil, nil)
	if css.Code != http.StatusOK {
		t.Fatalf("console.css: %d", css.Code)
	}
	for _, want := range []string{".body--auth .console", "--wordmark-bg", ".theme-switcher {", "flex-wrap: nowrap", ".theme-switcher__btn.is-active", "border: 0", "box-shadow: none", ".upload-progress__bar"} {
		if !strings.Contains(css.Body.String(), want) {
			t.Errorf("console.css missing %q", want)
		}
	}
	if cc := css.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("console.css Cache-Control = %q, want no-store", cc)
	}
}

// doFormRaw 不自动注入 CSRF（用于测试 403 场景）
func doFormRaw(
	t *testing.T,
	router *gin.Engine,
	method string,
	path string,
	form url.Values,
	session *http.Cookie,
	csrf *http.Cookie,
) *httptest.ResponseRecorder {
	t.Helper()
	body := ""
	if form != nil {
		body = form.Encode()
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if session != nil {
		req.AddCookie(session)
	}
	if csrf != nil {
		req.AddCookie(csrf)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

var _ = gin.Mode

// TestWebConsoleDeviceAuthorizationWorkflow 覆盖设备页，pending 先批准，
// approved 再整体替换授权，设备名称保持批准时的值
func TestWebConsoleDeviceAuthorizationWorkflow(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	if _, err := auth.CreateAccount(db, "root", "root-password-123", "admin"); err != nil {
		t.Fatal(err)
	}
	token := registerAndLogin(t, router, "owner-dev", "password123")
	vaultID := defaultVaultIDFromAPI(t, router, token)

	// 通过带设备头的登录请求登记设备（pending）
	code, login := loginAsDevice(t, router, "owner-dev", "password123", "desktop-2", "桌面机")
	if code != http.StatusOK || login["device_status"] != "pending" {
		t.Fatalf("device login: %d %v", code, login)
	}
	var owner models.User
	if err := db.Where("username = ?", "owner-dev").First(&owner).Error; err != nil {
		t.Fatal(err)
	}

	session, csrf := webLogin(t, router, "owner-dev", "password123")

	// pending 页面用名称输入替换静态名称，客户端 ID 保留为输入框下方的元数据
	page := doForm(t, router, http.MethodGet, "/dashboard/devices", nil, session, csrf)
	if page.Code != http.StatusOK {
		t.Fatalf("devices page: %d", page.Code)
	}
	if strings.Contains(page.Body.String(), `<select name="vault_ids"`) {
		t.Fatal("devices page must not render a multi-select for vault authorization")
	}
	body := page.Body.String()
	if !strings.Contains(body, "<th>操作</th>") ||
		!strings.Contains(body, `class="device-auth-summary"`) ||
		!strings.Contains(body, `class="device-auth-summary__actions"`) ||
		!strings.Contains(body, `id="device-approve-desktop-2"`) ||
		strings.Contains(body, `data-modal-open="device-modal-desktop-2"`) ||
		!strings.Contains(body, `form="device-revoke-desktop-2"`) ||
		!strings.Contains(body, `id="device-revoke-desktop-2"`) {
		t.Fatalf("device authorization must use a summary, rightmost actions, and modal: %s", page.Body)
	}
	deviceCellStart := strings.Index(body, `class="device-cell"`)
	deviceCellEnd := -1
	if deviceCellStart >= 0 {
		deviceCellEnd = strings.Index(body[deviceCellStart:], `</td>`)
	}
	deviceCell := ""
	if deviceCellStart >= 0 && deviceCellEnd >= 0 {
		deviceCell = body[deviceCellStart : deviceCellStart+deviceCellEnd]
	}
	nameInput := strings.Index(deviceCell, `class="device-name-input"`)
	clientID := strings.Index(deviceCell, `desktop-2`)
	if deviceCellStart < 0 || deviceCellEnd < 0 || nameInput < 0 ||
		!strings.Contains(page.Body.String(), "批准设备") {
		t.Fatalf("pending device page must replace the static name with the identity input: %s", page.Body)
	}
	if strings.Contains(deviceCell, `<strong>桌面机</strong>`) || clientID < 0 || nameInput > clientID {
		t.Fatalf("pending identity must render input before client ID without a duplicate static name: %s", deviceCell)
	}

	// pending 名称仍执行 1-128 字符校验
	longName := strings.Repeat("长", 129)
	bad := doForm(t, router, http.MethodPost, "/dashboard/devices/desktop-2/authorize",
		url.Values{"name": {longName}, "status": {"approved"}, "vault_ids": {vaultID}}, session, csrf)
	if bad.Code != http.StatusSeeOther || !strings.Contains(bad.Header().Get("Location"), "error=") {
		t.Fatalf("invalid pending name must be rejected: %d %q", bad.Code, bad.Header().Get("Location"))
	}

	// pending 只提交批准与设备名称，不依赖仓库授权
	approve := doForm(t, router, http.MethodPost, "/dashboard/devices/desktop-2/approve",
		url.Values{"name": {"新笔记本"}}, session, csrf)
	if approve.Code != http.StatusSeeOther {
		t.Fatalf("approve+rename: %d body=%s", approve.Code, approve.Body)
	}
	var dev models.ClientDevice
	if err := db.Where("user_id = ? AND client_id = ?", owner.ID, "desktop-2").First(&dev).Error; err != nil {
		t.Fatal(err)
	}
	if dev.Status != "approved" || dev.Name != "新笔记本" {
		t.Fatalf("device after save: %#v", dev)
	}
	var access models.DeviceVaultAccess
	if err := db.Where("user_id = ? AND client_id = ? AND vault_id = ?", owner.ID, "desktop-2", vaultID).
		First(&access).Error; err == nil {
		t.Fatal("approval must not grant vault access")
	}
	authorize := doForm(t, router, http.MethodPost, "/dashboard/devices/desktop-2/authorize",
		url.Values{"vault_ids": {vaultID}}, session, csrf)
	if authorize.Code != http.StatusSeeOther {
		t.Fatalf("authorize approved device: %d body=%s", authorize.Code, authorize.Body)
	}
	if err := db.Where("user_id = ? AND client_id = ? AND vault_id = ?", owner.ID, "desktop-2", vaultID).
		First(&access).Error; err != nil {
		t.Fatalf("authorized vault missing: %v", err)
	}

	// approved 只更新授权；即使伪造 name 字段也不能改名
	otherVault := createVaultViaAPI(t, router, token, "Second Vault")
	update := doForm(t, router, http.MethodPost, "/dashboard/devices/desktop-2/authorize",
		url.Values{
			"name":      {"改名设备"},
			"vault_ids": {otherVault},
		}, session, csrf)
	if update.Code != http.StatusSeeOther {
		t.Fatalf("update device: %d body=%s", update.Code, update.Body)
	}
	if err := db.Where("user_id = ? AND client_id = ?", owner.ID, "desktop-2").First(&dev).Error; err != nil {
		t.Fatal(err)
	}
	if dev.Name != "新笔记本" {
		t.Fatalf("approved device name changed: %q", dev.Name)
	}
	var count int64
	db.Model(&models.DeviceVaultAccess{}).
		Where("user_id = ? AND client_id = ?", owner.ID, "desktop-2").Count(&count)
	if count != 1 {
		t.Fatalf("expected exactly 1 access after replace, got %d", count)
	}
	if err := db.Where("user_id = ? AND client_id = ? AND vault_id = ?", owner.ID, "desktop-2", otherVault).
		First(&models.DeviceVaultAccess{}).Error; err != nil {
		t.Fatalf("new vault access missing: %v", err)
	}
	if err := db.Where("user_id = ? AND client_id = ? AND vault_id = ?", owner.ID, "desktop-2", vaultID).
		First(&models.DeviceVaultAccess{}).Error; err == nil {
		t.Fatal("old vault access not replaced")
	}

	approvedPage := doForm(t, router, http.MethodGet, "/dashboard/devices", nil, session, csrf)
	if strings.Contains(approvedPage.Body.String(), `name="name"`) {
		t.Fatalf("approved device page must not render a name input: %s", approvedPage.Body)
	}
	if !strings.Contains(approvedPage.Body.String(), `<strong>新笔记本</strong>`) ||
		!strings.Contains(approvedPage.Body.String(), `desktop-2`) {
		t.Fatalf("approved device page must restore the saved name and client ID: %s", approvedPage.Body)
	}
}

// TestWebConsoleDevicesEmptyVaultState 覆盖无仓库时的页面空态
func TestWebConsoleDevicesEmptyVaultState(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	if _, err := auth.CreateAccount(db, "novault-user", "password123", "user"); err != nil {
		t.Fatal(err)
	}
	code, _ := loginAsDevice(t, router, "novault-user", "password123", "desktop-empty", "空仓库设备")
	if code != http.StatusOK {
		t.Fatalf("device login: %d", code)
	}
	session, csrf := webLogin(t, router, "novault-user", "password123")
	page := doForm(t, router, http.MethodGet, "/dashboard/devices", nil, session, csrf)
	if page.Code != http.StatusOK {
		t.Fatalf("devices page: %d", page.Code)
	}
	if !strings.Contains(page.Body.String(), "暂无可授权仓库，请先创建仓库") {
		t.Fatalf("empty vault state missing: %s", page.Body)
	}
	if strings.Contains(page.Body.String(), `<select name="vault_ids"`) {
		t.Fatal("must not render a misleading empty vault multi-select")
	}
	if !strings.Contains(page.Body.String(), `class="device-auth-summary"`) ||
		!strings.Contains(page.Body.String(), `id="device-approve-desktop-empty"`) ||
		!strings.Contains(page.Body.String(), `form="device-revoke-desktop-empty"`) ||
		strings.Contains(page.Body.String(), `data-modal-open="device-modal-desktop-empty"`) {
		t.Fatalf("empty vault authorization must use a one-line summary without an empty modal: %s", page.Body)
	}
}

func TestAdminCanApproveDeviceWithoutVault(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	if _, err := auth.CreateAccount(db, "root-admin", "root-password-123", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.CreateAccount(db, "no-vault-user", "password123", "user"); err != nil {
		t.Fatal(err)
	}
	code, _ := loginAsDevice(t, router, "no-vault-user", "password123", "desktop-no-vault", "无仓库设备")
	if code != http.StatusOK {
		t.Fatalf("device login: %d", code)
	}
	var owner models.User
	if err := db.Where("username = ?", "no-vault-user").First(&owner).Error; err != nil {
		t.Fatal(err)
	}
	adminSession, adminCSRF := webLogin(t, router, "root-admin", "root-password-123")
	page := doForm(t, router, http.MethodGet, "/dashboard/admin/devices", nil, adminSession, adminCSRF)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `id="device-approve-`+strconv.FormatUint(uint64(owner.ID), 10)+`-desktop-no-vault"`) {
		t.Fatalf("admin page must expose approval without a vault: %d %s", page.Code, page.Body)
	}
	approve := doForm(t, router, http.MethodPost, "/dashboard/admin/devices/desktop-no-vault/approve",
		url.Values{"user_id": {strconv.FormatUint(uint64(owner.ID), 10)}}, adminSession, adminCSRF)
	if approve.Code != http.StatusSeeOther {
		t.Fatalf("approve device without vault: %d %s", approve.Code, approve.Body)
	}
	var device models.ClientDevice
	if err := db.Where("user_id = ? AND client_id = ?", owner.ID, "desktop-no-vault").First(&device).Error; err != nil {
		t.Fatal(err)
	}
	if device.Status != deviceauth.DeviceStatusApproved {
		t.Fatalf("device status = %q, want approved", device.Status)
	}
	var accessCount int64
	db.Model(&models.DeviceVaultAccess{}).Where("user_id = ? AND client_id = ?", owner.ID, "desktop-no-vault").Count(&accessCount)
	if accessCount != 0 {
		t.Fatalf("approval without vault must not create vault access, got %d", accessCount)
	}
}

// TestWebConsoleDeviceListsExcludeRevokedDevices 覆盖吊销后设备只保留在审计数据中，
// 用户与管理员可操作列表及计数均不再出现
func TestWebConsoleDeviceListsExcludeRevokedDevices(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	if _, err := auth.CreateAccount(db, "root-admin", "root-password-123", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.CreateAccount(db, "device-owner", "password123", "user"); err != nil {
		t.Fatal(err)
	}
	code, _ := loginAsDevice(t, router, "device-owner", "password123", "revoked-device", "待吊销设备")
	if code != http.StatusOK {
		t.Fatalf("device login: %d", code)
	}

	userSession, userCSRF := webLogin(t, router, "device-owner", "password123")
	adminSession, adminCSRF := webLogin(t, router, "root-admin", "root-password-123")
	for name, page := range map[string]*httptest.ResponseRecorder{
		"user":  doForm(t, router, http.MethodGet, "/dashboard/devices", nil, userSession, userCSRF),
		"admin": doForm(t, router, http.MethodGet, "/dashboard/admin/devices", nil, adminSession, adminCSRF),
	} {
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "revoked-device") {
			t.Fatalf("%s page must list active device before revoke: %d %s", name, page.Code, page.Body)
		}
	}

	revoke := doForm(t, router, http.MethodPost, "/dashboard/devices/revoked-device/revoke", nil, userSession, userCSRF)
	if revoke.Code != http.StatusSeeOther {
		t.Fatalf("revoke device: %d body=%s", revoke.Code, revoke.Body)
	}
	var stored models.ClientDevice
	if err := db.Where("client_id = ?", "revoked-device").First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != deviceauth.DeviceStatusRevoked {
		t.Fatalf("stored status = %q, want revoked", stored.Status)
	}

	for name, page := range map[string]*httptest.ResponseRecorder{
		"user":  doForm(t, router, http.MethodGet, "/dashboard/devices", nil, userSession, userCSRF),
		"admin": doForm(t, router, http.MethodGet, "/dashboard/admin/devices", nil, adminSession, adminCSRF),
	} {
		body := page.Body.String()
		if page.Code != http.StatusOK || strings.Contains(body, "revoked-device") ||
			!strings.Contains(body, `class="panel-empty"`) {
			t.Fatalf("%s page must exclude revoked device and render empty state: %d %s", name, page.Code, page.Body)
		}
	}
}

// TestAdminDeviceAuthorizationRejectsInaccessibleVault 覆盖管理员为跨用户设备授权的场景，
// 目标用户无权访问的仓库必须被拒绝，不因仓库存在而放行
func TestAdminDeviceAuthorizationRejectsInaccessibleVault(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, db, _ := newTestServer(t)
	router := srv.Router()
	if _, err := auth.CreateAccount(db, "root-admin", "root-password-123", "admin"); err != nil {
		t.Fatal(err)
	}
	ownerToken := registerAndLogin(t, router, "owner-a", "password123")
	ownerVault := defaultVaultIDFromAPI(t, router, ownerToken)

	memberToken := registerAndLogin(t, router, "member-b", "password123")
	memberVault := defaultVaultIDFromAPI(t, router, memberToken)
	if ownerVault == memberVault {
		t.Fatal("vaults must be distinct")
	}
	var member models.User
	if err := db.Where("username = ?", "member-b").First(&member).Error; err != nil {
		t.Fatal(err)
	}
	code, login := loginAsDevice(t, router, "member-b", "password123", "member-dev", "Member PC")
	if code != http.StatusOK || login["device_status"] != "pending" {
		t.Fatalf("member device login: %d %v", code, login)
	}

	rootSession, rootCSRF := webLogin(t, router, "root-admin", "root-password-123")
	approve := doForm(t, router, http.MethodPost, "/dashboard/admin/devices/member-dev/approve",
		url.Values{
			"user_id": {strconv.FormatUint(uint64(member.ID), 10)},
			"name":    {"Member PC"},
		}, rootSession, rootCSRF)
	if approve.Code != http.StatusSeeOther || strings.Contains(approve.Header().Get("Location"), "error=") {
		t.Fatalf("member device approval must succeed before vault authorization: %d %q", approve.Code, approve.Header().Get("Location"))
	}

	// 管理员不能为 member 设备授权 owner 仓库
	denied := doForm(t, router, http.MethodPost, "/dashboard/admin/devices/member-dev/authorize",
		url.Values{
			"user_id":   {strconv.FormatUint(uint64(member.ID), 10)},
			"name":      {"Member PC"},
			"status":    {"approved"},
			"vault_ids": {ownerVault},
		}, rootSession, rootCSRF)
	if denied.Code != http.StatusSeeOther || !strings.Contains(denied.Header().Get("Location"), "error=") {
		t.Fatalf("inaccessible vault must be rejected: %d %q", denied.Code, denied.Header().Get("Location"))
	}
	var deniedCount int64
	db.Model(&models.DeviceVaultAccess{}).
		Where("user_id = ? AND client_id = ?", member.ID, "member-dev").Count(&deniedCount)
	if deniedCount != 0 {
		t.Fatalf("inaccessible vault must not be persisted, got %d rows", deniedCount)
	}

	// 管理员为 member 自己可访问的仓库授权成功
	allowed := doForm(t, router, http.MethodPost, "/dashboard/admin/devices/member-dev/authorize",
		url.Values{
			"user_id":   {strconv.FormatUint(uint64(member.ID), 10)},
			"name":      {"Member PC"},
			"status":    {"approved"},
			"vault_ids": {memberVault},
		}, rootSession, rootCSRF)
	if allowed.Code != http.StatusSeeOther || strings.Contains(allowed.Header().Get("Location"), "error=") {
		t.Fatalf("member vault must be allowed: %d %q", allowed.Code, allowed.Header().Get("Location"))
	}
	if err := db.Where("user_id = ? AND client_id = ? AND vault_id = ?", member.ID, "member-dev", memberVault).
		First(&models.DeviceVaultAccess{}).Error; err != nil {
		t.Fatalf("member vault access missing: %v", err)
	}
}
