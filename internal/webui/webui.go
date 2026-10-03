// Package webui 提供登录页面、控制台与管理员视图的渲染
package webui

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/auth"
	"github.com/helantianshen/oss-sync/internal/config"
	"github.com/helantianshen/oss-sync/internal/models"
	"github.com/helantianshen/oss-sync/internal/serverplugin"
	"github.com/helantianshen/oss-sync/internal/update"
	"github.com/helantianshen/oss-sync/internal/vaultaccess"
)

// sessionCookie 是登录后网页会话的 HttpOnly cookie
const (
	sessionCookie     = "oss_web_session"
	csrfCookie        = "oss_csrf"
	webLanguageCookie = "oss_web_language"
)

//go:embed templates/*.html templates/partials/*.html assets/*
var webFS embed.FS

// Handler 持有控制台依赖
type Handler struct {
	DB            *gorm.DB
	Cfg           *config.Config
	tpl           *template.Template
	loginLimit    *auth.AttemptLimiter
	registerLimit *auth.AttemptLimiter
	updater       *update.Updater
	updateSvc     *update.Service
	pluginManager *serverplugin.Manager
	fileWriter    fileContentWriter
}

// SetUpdateService 注入共享更新服务（直接注入，不代理 Bearer token）
func (h *Handler) SetUpdateService(svc *update.Service, up *update.Updater) {
	h.updateSvc = svc
	h.updater = up
}

// SetPluginManager 注入服务插件管理器
func (h *Handler) SetPluginManager(manager *serverplugin.Manager) {
	h.pluginManager = manager
}

// SetFileWriter 注入文件写入实现，供控制台内置编辑器复用真实同步写入管线
func (h *Handler) SetFileWriter(writer fileContentWriter) {
	h.fileWriter = writer
}

// layoutData 是所有控制台页面共用的外壳数据
type layoutData struct {
	Page             string // 要渲染的页面模板名，如 "overview"
	Title            string
	Username         string
	IsAdmin          bool
	CSRF             string
	ShowSidebar      bool // 登录/注册页为 false
	ActiveGroup      string
	ActivePage       string
	ActivePluginID   string
	PluginSettings   []pluginNav
	PluginAdminPages []serverplugin.PluginAdminPage
	CurrentVault     *vaultNav  // 当前仓库页的上下文
	NavVaults        []vaultNav // 侧边栏仓库导航，所有控制台页面可见
	Flash            string
	FlashKind        string // success 或 error
	ConsoleThemeName string
	Language         string
	CSPNonce         string
	ContentHTML      template.HTML
	// Pagination 是控制台列表共享的分页控件数据
	Pagination Page
	// PaginationBasePath 是分页链接的基础路径，PageURL 依此构造页码链接
	PaginationBasePath string
	// PageSizeOptions 是用户可选的每页条数
	PageSizeOptions []int
	// SelectedPageSize 是当前生效的每页条数
	SelectedPageSize int
	// Query 是当前请求的查询参数，供分页链接与每页条数表单保留过滤条件
	Query url.Values
}

func (ld layoutData) T(key string, args ...any) string {
	return translate(ld.Language, key, args...)
}

// vaultNav 侧边栏"当前仓库"菜单的上下文
type vaultNav struct {
	ID   string
	Name string
}

type pluginNav struct {
	ID   string
	Name string
}

// templateFuncs 返回控制台模板共用的函数表
// 新增模板函数时必须同步这里，否则各测试的独立 FuncMap 会漏定义
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"formatBytes": formatBytes,
		"timeFmt": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		"sub":      func(a, b int) int { return a - b },
		"add":      func(a, b int) int { return a + b },
		"urlquery": url.QueryEscape,
		"PageURL":  PageURL,
	}
}

// New 解析控制台模板并创建网页处理器
func New(db *gorm.DB, cfg *config.Config) (*Handler, error) {
	funcs := templateFuncs()
	tpl, err := template.New("web").Funcs(funcs).ParseFS(webFS,
		"templates/layout.html",
		"templates/partials/*.html",
		"templates/*.html",
	)
	if err != nil {
		return nil, fmt.Errorf("parse web UI templates: %w", err)
	}
	return &Handler{
		DB: db, Cfg: cfg, tpl: tpl,
		loginLimit:    auth.NewAttemptLimiter(8, time.Minute),
		registerLimit: auth.NewAttemptLimiter(5, time.Minute),
	}, nil
}

// Register 注册公开页面、登录会话和受保护的控制台路由
func (h *Handler) Register(r *gin.Engine) {
	r.GET("/ui/assets/console.css", h.styles)
	r.GET("/ui/assets/app.js", h.script("app.js", "text/javascript; charset=utf-8"))
	r.GET("/ui/assets/metrics.js", h.script("metrics.js", "text/javascript; charset=utf-8"))
	r.GET("/ui/assets/theme.js", h.script("theme.js", "text/javascript; charset=utf-8"))
	r.GET("/ui/themes/:theme/*filepath", h.consoleThemeAsset)

	// 登录、注册、登出（公开）
	r.GET("/login", h.loginPage)
	r.POST("/login", h.loginSubmit)
	r.GET("/register", h.registerPage)
	r.POST("/register", h.registerSubmit)
	r.POST("/logout", h.logout)

	// 受保护的控制台
	console := r.Group("/dashboard", h.requireSession)
	{
		console.GET("", h.overviewPage)
		console.GET("/metrics", h.systemMetricsPage)
		console.GET("/vaults", h.vaultsPage)
		console.POST("/vaults", h.createVault)
		console.GET("/vaults/new", h.newVaultPage)
		console.GET("/vaults/:vault_id", h.vaultFilesPage)
		console.POST("/vaults/:vault_id/files/delete", h.deleteFile)
		console.GET("/vaults/:vault_id/files/preview", h.previewFile)
		console.GET("/vaults/:vault_id/files/download", h.downloadFile)
		console.GET("/vaults/:vault_id/files/sandbox", h.sandboxPreviewFile)
		console.GET("/vaults/:vault_id/files/edit", h.editFilePage)
		console.POST("/vaults/:vault_id/files/edit", h.saveFileEdit)
		console.GET("/vaults/:vault_id/shares", h.sharesPage)
		console.POST("/vaults/:vault_id/shares", h.createShare)
		console.POST("/vaults/:vault_id/shares/:share_id/allow_copy", h.toggleShareCopy)
		console.POST("/vaults/:vault_id/shares/:share_id/delete", h.deleteShare)
		console.GET("/vaults/:vault_id/recycle", h.recyclePage)
		console.POST("/vaults/:vault_id/recycle/:file_id/restore", h.restoreRecycle)
		console.POST("/vaults/:vault_id/recycle/:file_id/delete", h.purgeRecycle)
		console.GET("/vaults/:vault_id/history", h.historyPage)
		console.GET("/vaults/:vault_id/history/:history_id", h.historyDetailPage)
		console.POST("/vaults/:vault_id/history/:history_id/restore", h.restoreHistory)
		console.GET("/vaults/:vault_id/members", h.membersPage)
		console.POST("/vaults/:vault_id/members", h.addMember)
		console.POST("/vaults/:vault_id/members/:user_id/role", h.updateMemberRole)
		console.POST("/vaults/:vault_id/members/:user_id/delete", h.removeMember)
		console.POST("/vaults/:vault_id/members/:user_id/collaborations/revoke", h.revokeMemberCollaborations)
		console.GET("/vaults/:vault_id/settings", h.vaultSettingsPage)
		console.POST("/vaults/:vault_id/settings", h.saveVaultSettings)
		console.GET("/vaults/:vault_id/plugins/:plugin_id/settings", h.pluginSettingsPage)
		console.POST("/vaults/:vault_id/plugins/:plugin_id/settings", h.savePluginSettings)
		console.GET("/plugins/:plugin_id/settings", h.pluginSettingsGlobalPage)
		console.POST("/plugins/:plugin_id/settings", h.savePluginSettingsGlobal)
		console.GET("/vaults/:vault_id/papertrail", func(c *gin.Context) {
			c.Redirect(http.StatusMovedPermanently, "/dashboard/plugins/papertrail-settings/settings?vault_id="+url.QueryEscape(c.Param("vault_id")))
		})
		console.POST("/vaults/:vault_id/delete", h.deleteVault)
		console.GET("/devices", h.devicesPage)
		console.POST("/devices/:client_id/approve", h.approveDevice)
		console.POST("/devices/:client_id/rename", h.renameDevice)
		console.POST("/devices/:client_id/authorize", h.authorizeDevice)
		console.POST("/devices/:client_id/revoke", h.revokeDevice)
		console.GET("/account", h.accountPage)
		console.POST("/account/settings", h.saveAccountSettings)
		console.POST("/account/language", h.saveAccountLanguage)
		console.POST("/account/theme", h.saveConsoleTheme)
		console.POST("/account/password", h.changePassword)
	}

	// 管理员控制台
	adminGroup := console.Group("/admin", h.requireAdmin)
	{
		adminGroup.GET("", h.adminUsersPage)
		adminGroup.POST("/users/:user_id/role", h.adminSetUserRole)
		adminGroup.POST("/users/:user_id/reset-password", h.adminResetPassword)
		adminGroup.POST("/users/:user_id/delete", h.adminDeleteUser)
		adminGroup.GET("/vaults", h.adminVaultsPage)
		adminGroup.GET("/vaults/:vault_id", h.adminVaultDetailPage)
		adminGroup.GET("/devices", h.adminDevicesPage)
		adminGroup.POST("/devices/:client_id/approve", h.adminApproveDevice)
		adminGroup.POST("/devices/:client_id/authorize", h.adminAuthorizeDevice)
		adminGroup.POST("/devices/:client_id/revoke", h.adminRevokeDevice)
		adminGroup.GET("/system", h.adminSystemPage)
		adminGroup.POST("/system", h.adminSaveSystem)
		adminGroup.GET("/data", h.adminDataPage)
		adminGroup.POST("/system/database", h.adminSaveDatabase)
		adminGroup.GET("/system/update/status", h.adminUpdateStatusJSON)
		adminGroup.POST("/system/update/check", h.adminUpdateCheck)
		adminGroup.POST("/system/update", h.adminUpdateTrigger)
		adminGroup.GET("/plugins", h.adminPluginsPage)
		adminGroup.GET("/plugins/guide.md", h.adminPluginGuideMarkdown)
		adminGroup.POST("/plugins/upload", h.adminPluginUpload)
		adminGroup.POST("/plugins/:id/enable", h.adminPluginEnable)
		adminGroup.POST("/plugins/:id/disable", h.adminPluginDisable)
		adminGroup.POST("/plugins/:id/delete", h.adminPluginDelete)
		adminGroup.POST("/plugins/:id/files/save", h.adminPluginFileSave)
		adminGroup.GET("/plugins/:id/page/:slug", h.adminPluginPage)
		adminGroup.POST("/plugins/:id/update-check", h.adminPluginUpdateCheck)
		adminGroup.POST("/plugins/:id/update", h.adminPluginUpdate)
		adminGroup.GET("/backups/:id/download", h.downloadBackup)
		adminGroup.POST("/backups/:id/delete", h.deleteBackup)
	}

	// 旧 /admin 路由保留重定向，统一指向控制台与登录入口
	r.GET("/admin/login", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/login")
	})
	r.GET("/admin", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/dashboard/admin")
	})
	r.GET("/admin/vaults/:vault_id", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/dashboard/vaults/"+url.PathEscape(c.Param("vault_id")))
	})
	r.POST("/admin/login", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/login")
	})
	r.POST("/admin/logout", h.logout)
}

// sessionUser 从网页会话 Cookie 解析已登录用户
func (h *Handler) sessionUser(c *gin.Context) *models.User {
	token, err := c.Cookie(sessionCookie)
	if err != nil || token == "" {
		return nil
	}
	user, err := auth.AuthenticateToken(h.DB, h.Cfg, token)
	if err != nil {
		return nil
	}
	return user
}

// setSessionCookie 设置登录会话 cookie 与 CSRF cookie
func (h *Handler) setSessionCookie(c *gin.Context, user *models.User) {
	token, expiresIn, err := auth.IssueWebToken(h.Cfg, *user)
	if err != nil {
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(expiresIn),
		HttpOnly: true,
		Secure:   requestIsHTTPS(c),
		SameSite: http.SameSiteLaxMode,
	})
	// CSRF token 与会话同生命周期
	if _, err := c.Cookie(csrfCookie); err != nil {
		http.SetCookie(c.Writer, &http.Cookie{
			Name:     csrfCookie,
			Value:    randomToken(),
			Path:     "/",
			MaxAge:   int(expiresIn),
			HttpOnly: false,
			Secure:   requestIsHTTPS(c),
			SameSite: http.SameSiteLaxMode,
		})
	}
}

func clearSessionCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.SetCookie(c.Writer, &http.Cookie{Name: csrfCookie, Value: "", Path: "/", MaxAge: -1, SameSite: http.SameSiteLaxMode})
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// requireSession 要求已登录，并校验状态修改请求的 CSRF token
func (h *Handler) requireSession(c *gin.Context) {
	user := h.sessionUser(c)
	if user == nil {
		requestedWebLanguage(c)
		c.Redirect(http.StatusSeeOther, "/login")
		c.Abort()
		return
	}
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		if !h.validCSRF(c) {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
	}
	c.Set("oss.web_user", user)
	c.Next()
}

func (h *Handler) requireAdmin(c *gin.Context) {
	user, _ := c.Get("oss.web_user")
	u, _ := user.(*models.User)
	if u == nil || u.Role != "admin" {
		c.Redirect(http.StatusSeeOther, "/dashboard")
		c.Abort()
		return
	}
	c.Next()
}

func (h *Handler) validCSRF(c *gin.Context) bool {
	expected, err := c.Cookie(csrfCookie)
	if err != nil || expected == "" {
		return false
	}
	got := c.PostForm("_csrf")
	if got == "" {
		got = c.GetHeader("X-CSRF-Token")
	}
	return got != "" && got == expected
}

func (h *Handler) webUser(c *gin.Context) *models.User {
	user, _ := c.Get("oss.web_user")
	u, _ := user.(*models.User)
	return u
}

func (h *Handler) userLang(c *gin.Context) string {
	if language := requestedWebLanguage(c); language != "" {
		return language
	}
	if u := h.webUser(c); u != nil {
		return h.selectedWebLanguage(u.ID)
	}
	return defaultWebLanguage
}

func cookieValue(c *gin.Context, name string) string {
	value, err := c.Cookie(name)
	if err != nil {
		return ""
	}
	return value
}

func requestedWebLanguage(c *gin.Context) string {
	for _, candidate := range []string{c.Query("lang"), cookieValue(c, webLanguageCookie)} {
		if candidate == "zh" || candidate == "en" {
			return candidate
		}
	}
	accept := c.GetHeader("Accept-Language")
	if accept == "" {
		return ""
	}
	// 遍历所有语言段，按 q 值选择客户端偏好最高的支持语言
	// q=0 表示客户端明确不接受该语言，必须排除
	bestLang := ""
	bestQ := -1.0
	for _, part := range strings.Split(accept, ",") {
		piece := strings.TrimSpace(part)
		q := 1.0
		if idx := strings.Index(piece, ";"); idx >= 0 {
			params := piece[idx+1:]
			piece = piece[:idx]
			if qi := strings.Index(params, "q="); qi >= 0 {
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(params[qi+2:]), 64); err == nil {
					q = parsed
				}
			}
		}
		tag := strings.TrimSpace(piece)
		// 取主语言子标签：zh-CN → zh, en-US → en
		if idx := strings.Index(tag, "-"); idx > 0 {
			tag = tag[:idx]
		}
		tag = strings.ToLower(tag)
		if tag != "zh" && tag != "en" {
			continue
		}
		if q > bestQ {
			bestLang = tag
			bestQ = q
		}
	}
	if bestQ <= 0 {
		return ""
	}
	return bestLang
}

func (h *Handler) t(c *gin.Context, key string, args ...any) string {
	return translate(h.userLang(c), key, args...)
}

// 渲染

// paginatedData 由需要分页控件的页面数据实现，render 据此填充布局分页字段
type paginatedData interface {
	paginationPage() Page
	paginationBasePath() string
}

func (h *Handler) applyPagination(c *gin.Context, ld *layoutData, data any) {
	ld.Query = c.Request.URL.Query()
	ld.PageSizeOptions = AllowedPageSizes
	ld.SelectedPageSize = DefaultPageSize
	paginated, ok := data.(paginatedData)
	if !ok {
		return
	}
	ld.Pagination = paginated.paginationPage()
	ld.SelectedPageSize = ld.Pagination.PageSize
	ld.PaginationBasePath = paginated.paginationBasePath()
}

// render 使用统一布局渲染控制台页面；page 为页面模板名
func (h *Handler) render(c *gin.Context, status int, page, title, activeGroup, activePage string, data any) {
	u := h.webUser(c)
	ld := layoutData{Page: page, Title: title, Username: "", IsAdmin: false, ShowSidebar: u != nil, ActiveGroup: activeGroup, ActivePage: activePage}
	h.applyPagination(c, &ld, data)
	if u != nil {
		ld.Username = u.Username
		ld.IsAdmin = u.Role == "admin"
		ld.ConsoleThemeName = h.selectedConsoleTheme(u.ID)
		if selected, disabledTheme := h.selectedConsoleThemeState(u.ID); disabledTheme != "" {
			ld.ConsoleThemeName = selected
			ld.Flash = h.t(c, "admin.plugin_theme_disabled", disabledTheme)
			ld.FlashKind = "error"
		}
		ld.Language = h.userLang(c)
		ld.NavVaults = h.accessibleVaults(u)
		h.setPluginNavigationForUser(&ld, u)
		if ld.IsAdmin && h.pluginManager != nil {
			ld.PluginAdminPages = h.pluginManager.AdminPages()
		}
	}
	if token, err := c.Cookie(csrfCookie); err == nil {
		ld.CSRF = token
	}
	h.renderWithLayout(c, status, ld, data)
}

// setPluginNavigationForUser 构造插件设置导航
// 插件通过 manifest 的 settings_visibility 声明可见范围：
//   - always：插件启用即显示，不依赖模板或主题是否被选用（纯功能插件）
//   - when_used 或缺省：仅当插件的博客/控制台主题资源被某个仓库选用时显示
func (h *Handler) setPluginNavigationForUser(ld *layoutData, u *models.User) {
	if u == nil {
		return
	}
	usedThemes := h.usedThemeNames(u)
	for _, manifest := range serverplugin.BuiltinManifests() {
		if len(manifest.Settings) == 0 {
			continue
		}
		if !manifest.SettingsVisibleWhenUsed() {
			ld.PluginSettings = append(ld.PluginSettings, pluginNav{ID: manifest.ID, Name: manifest.Name})
			continue
		}
		if theme := builtinThemeForBuiltinPlugin(manifest.ID); theme != "" && usedThemes[theme] {
			ld.PluginSettings = append(ld.PluginSettings, pluginNav{ID: manifest.ID, Name: manifest.Name})
		}
	}
	var plugins []models.ServerPlugin
	if err := h.DB.Where("enabled = ?", true).Order("id asc").Find(&plugins).Error; err != nil {
		return
	}
	for _, plugin := range plugins {
		manifest, err := serverplugin.ParseManifest([]byte(plugin.ManifestJSON))
		if err != nil {
			// manifest 解析失败时仍允许插件运行期注册的设置生效
			manifest = serverplugin.Manifest{ID: plugin.ID, Name: plugin.Name}
		}
		if h.pluginManager != nil {
			if registration, ok := h.pluginManager.RegistrationFor(plugin.ID); ok && len(registration.Settings) > 0 {
				manifest.Settings = registration.Settings
			}
		}
		if len(manifest.Settings) == 0 {
			continue
		}
		if !manifest.SettingsVisibleWhenUsed() {
			ld.PluginSettings = append(ld.PluginSettings, pluginNav{ID: plugin.ID, Name: plugin.Name})
			continue
		}
		if !manifest.HasThemeResources() {
			ld.PluginSettings = append(ld.PluginSettings, pluginNav{ID: plugin.ID, Name: plugin.Name})
			continue
		}
		if h.pluginThemeInUse(manifest, usedThemes) {
			ld.PluginSettings = append(ld.PluginSettings, pluginNav{ID: plugin.ID, Name: plugin.Name})
		}
	}
}

// usedThemeNames 返回当前用户可访问仓库已选用的博客与控制台主题名集合
func (h *Handler) usedThemeNames(u *models.User) map[string]bool {
	used := make(map[string]bool)
	var vaultIDs []string
	for _, vault := range h.accessibleVaults(u) {
		vaultIDs = append(vaultIDs, vault.ID)
	}
	if len(vaultIDs) == 0 {
		return used
	}
	var settings []models.VaultSetting
	if err := h.DB.Where("vault_id IN ?", vaultIDs).Find(&settings).Error; err != nil {
		return used
	}
	for _, setting := range settings {
		if setting.ThemeName != "" {
			used[setting.ThemeName] = true
		}
	}
	var userSettings []models.UserSetting
	if err := h.DB.Where("user_id = ?", u.ID).Find(&userSettings).Error; err == nil {
		for _, setting := range userSettings {
			if setting.ConsoleThemeName != "" {
				used[setting.ConsoleThemeName] = true
			}
		}
	}
	return used
}

// pluginThemeInUse 判断插件的主题资源是否有任意一个被选用
// 仓库保存的 theme_name 是 ThemeResource.Key(pluginID)（形如 <plugin>--<resource>），
// 同时兼容早期直接按资源名保存的数据
func (h *Handler) pluginThemeInUse(manifest serverplugin.Manifest, usedThemes map[string]bool) bool {
	for _, theme := range manifest.BlogThemes {
		if usedThemes[theme.Key(manifest.ID)] || usedThemes[theme.Name] || usedThemes[theme.ID] {
			return true
		}
	}
	for _, theme := range manifest.ConsoleThemes {
		if usedThemes[theme.Key(manifest.ID)] || usedThemes[theme.Name] || usedThemes[theme.ID] {
			return true
		}
	}
	return false
}

// accessibleVaults 返回当前用户可访问的仓库，默认仓库排在前面
func (h *Handler) accessibleVaults(u *models.User) []vaultNav {
	if u == nil {
		return nil
	}
	var owned []models.Vault
	if err := h.DB.Where("owner_id = ?", u.ID).
		Order("is_default desc, created_at asc").Find(&owned).Error; err != nil {
		return nil
	}
	seen := make(map[string]bool, len(owned))
	out := make([]vaultNav, 0, len(owned))
	for _, vault := range owned {
		seen[vault.ID] = true
		out = append(out, vaultNav{ID: vault.ID, Name: vault.Name})
	}
	var memberIDs []string
	if err := h.DB.Model(&models.VaultMember{}).
		Where("user_id = ? AND role IN ?", u.ID, []string{vaultaccess.RoleManager, vaultaccess.RoleParticipant}).
		Pluck("vault_id", &memberIDs).Error; err != nil || len(memberIDs) == 0 {
		return out
	}
	var shared []models.Vault
	if err := h.DB.Where("id IN ?", memberIDs).
		Order("is_default desc, created_at asc").Find(&shared).Error; err != nil {
		return out
	}
	for _, vault := range shared {
		if seen[vault.ID] {
			continue
		}
		seen[vault.ID] = true
		out = append(out, vaultNav{ID: vault.ID, Name: vault.Name})
	}
	return out
}

func pluginHasNoAssociations(db *gorm.DB, pluginID string) bool {
	var count int64
	return db.Model(&models.ServerPluginAssociation{}).Where("plugin_id = ?", pluginID).Count(&count).Error == nil && count == 0
}

// renderWithLayout 渲染页面内容并把结果注入统一布局
func (h *Handler) renderWithLayout(c *gin.Context, status int, ld layoutData, data any) {
	ld.CSPNonce = randomToken()
	pageData := struct {
		Layout layoutData
		Data   any
	}{Layout: ld, Data: data}
	var buf strings.Builder
	if err := h.tpl.ExecuteTemplate(&buf, ld.Page, pageData); err != nil {
		fmt.Fprintf(os.Stderr, "webui template %s: %v\n", ld.Page, err)
		buf.Reset()
		if err := h.tpl.ExecuteTemplate(&buf, "overview", pageData); err != nil {
			ld.ContentHTML = template.HTML("<p>template error</p>")
		}
	}
	ld.ContentHTML = template.HTML(buf.String())
	setPageHeaders(c, ld.CSPNonce)
	c.Status(status)
	_ = h.tpl.ExecuteTemplate(c.Writer, "layout", struct {
		Layout layoutData
		Data   any
	}{Layout: ld, Data: data})
}

func setPageHeaders(c *gin.Context, nonce string) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "same-origin")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Frame-Options", "DENY")
	scriptSource := "'self'"
	styleSource := "'self'"
	if nonce != "" {
		scriptSource += " 'nonce-" + nonce + "'"
		styleSource += " 'nonce-" + nonce + "'"
	}
	c.Header(
		"Content-Security-Policy",
		"default-src 'none'; connect-src 'self'; script-src "+scriptSource+"; style-src "+styleSource+"; img-src 'self' data: https:; "+
			"frame-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'",
	)
}

func requestIsHTTPS(c *gin.Context) bool {
	return c.Request.TLS != nil ||
		strings.EqualFold(strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")), "https")
}

// styles 返回控制台基础样式
func (h *Handler) styles(c *gin.Context) {
	raw, err := webFS.ReadFile("assets/console.css")
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/css; charset=utf-8", raw)
}

func (h *Handler) script(name, contentType string) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, err := webFS.ReadFile("assets/" + name)
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Data(http.StatusOK, contentType, raw)
	}
}

// loginView 是登录页的显示数据
type loginView struct {
	Error string
}

func (h *Handler) loginPage(c *gin.Context) {
	if h.sessionUser(c) != nil {
		c.Redirect(http.StatusFound, "/dashboard")
		return
	}
	h.renderAuth(c, http.StatusOK, "login", loginView{})
}

func (h *Handler) loginSubmit(c *gin.Context) {
	if !h.loginLimit.Allow("web-login:" + c.ClientIP()) {
		h.renderAuth(c, http.StatusTooManyRequests, "login", loginView{Error: h.t(c, "err.too_many_attempts")})
		return
	}
	user, err := auth.AuthenticateCredentials(h.DB, c.PostForm("username"), c.PostForm("password"))
	if err != nil {
		h.renderAuth(c, http.StatusUnauthorized, "login", loginView{Error: h.t(c, "err.invalid_credentials")})
		return
	}
	h.setSessionCookie(c, user)
	c.Redirect(http.StatusSeeOther, "/dashboard")
}

func (h *Handler) logout(c *gin.Context) {
	clearSessionCookie(c)
	c.Redirect(http.StatusSeeOther, "/login")
}

// renderAuth 渲染登录/注册等无侧边栏页面
func (h *Handler) renderAuth(c *gin.Context, status int, page string, data any) {
	language := requestedWebLanguage(c)
	if language == "" {
		language = defaultWebLanguage
	}
	ld := layoutData{Page: page, ShowSidebar: false, Language: language}
	if token, err := c.Cookie(csrfCookie); err == nil {
		ld.CSRF = token
	}
	h.renderWithLayout(c, status, ld, data)
}

// formatBytes 将字节数格式化为控制台容量文案
func formatBytes(size int64) string {
	const gib = 1024 * 1024 * 1024
	const mib = 1024 * 1024
	if size >= gib {
		return fmt.Sprintf("%.1f GiB", float64(size)/gib)
	}
	if size >= mib {
		return fmt.Sprintf("%.1f MiB", float64(size)/mib)
	}
	if size >= 1024 {
		return fmt.Sprintf("%.1f KiB", float64(size)/1024)
	}
	return fmt.Sprintf("%d B", size)
}
