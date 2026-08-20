// Package webui 提供统一登录、注册和带侧边栏的网页控制台。
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

	"github.com/oss/oss-server/internal/auth"
	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/models"
)

// sessionCookie 是登录后网页会话的 HttpOnly cookie。
const sessionCookie = "oss_web_session"

// csrfCookie 是 double-submit CSRF token cookie（非 HttpOnly，供 JS 读取）。
const csrfCookie = "oss_csrf"

//go:embed templates/*.html templates/partials/*.html assets/*
var webFS embed.FS

// Handler 持有控制台依赖。
type Handler struct {
	DB            *gorm.DB
	Cfg           *config.Config
	tpl           *template.Template
	loginLimit    *auth.AttemptLimiter
	registerLimit *auth.AttemptLimiter
}

// layoutData 是所有控制台页面共用的外壳数据。
type layoutData struct {
	Page             string // 要渲染的页面模板名，如 "overview"
	Title            string
	Username         string
	IsAdmin          bool
	CSRF             string
	ShowSidebar      bool // 登录/注册页为 false
	ActiveGroup      string
	ActivePage       string
	CurrentVault     *vaultNav // 进入仓库页后为当前仓库导航
	Flash            string
	FlashKind        string // success / error
	ConsoleThemeName string
	Language         string
	ContentHTML      template.HTML
}

func (ld layoutData) T(key string, args ...any) string {
	return translate(ld.Language, key, args...)
}

// vaultNav 侧边栏"当前仓库"菜单的上下文。
type vaultNav struct {
	ID                 string
	Name               string
	HasThemeSettings   bool
	ThemeSettingsLabel string
}

func New(db *gorm.DB, cfg *config.Config) (*Handler, error) {
	funcs := template.FuncMap{
		"formatBytes": formatBytes,
		"timeFmt": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		"sub":      func(a, b int) int { return a - b },
		"urlquery": url.QueryEscape,
	}
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

// Register 注册公开页面、登录会话和受保护的控制台路由。
func (h *Handler) Register(r *gin.Engine) {
	r.GET("/ui/assets/console.css", h.styles)
	r.GET("/ui/assets/app.js", h.script("app.js", "text/javascript; charset=utf-8"))
	r.GET("/ui/assets/metrics.js", h.script("metrics.js", "text/javascript; charset=utf-8"))
	r.GET("/ui/assets/theme.js", h.script("theme.js", "text/javascript; charset=utf-8"))
	r.GET("/ui/themes/:theme/*filepath", h.consoleThemeAsset)

	// 登录、注册、登出（公开）。
	r.GET("/login", h.loginPage)
	r.POST("/login", h.loginSubmit)
	r.GET("/register", h.registerPage)
	r.POST("/register", h.registerSubmit)
	r.POST("/logout", h.logout)

	// 受保护的控制台。
	console := r.Group("/dashboard", h.requireSession)
	{
		console.GET("", h.overviewPage)
		console.GET("/metrics", h.systemMetricsPage)
		console.GET("/vaults", h.vaultsPage)
		console.POST("/vaults", h.createVault)
		console.GET("/vaults/new", h.newVaultPage)
		console.GET("/vaults/:vault_id", h.vaultFilesPage)
		console.POST("/vaults/:vault_id/files/delete", h.deleteFile)
		console.GET("/vaults/:vault_id/files/preview", h.previewMarkdownFile)
		console.GET("/vaults/:vault_id/files/download", h.downloadFile)
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
		console.GET("/vaults/:vault_id/theme-settings", h.themeSettingsPage)
		console.POST("/vaults/:vault_id/theme-settings", h.saveThemeSettings)
		console.GET("/vaults/:vault_id/papertrail", func(c *gin.Context) {
			c.Redirect(http.StatusMovedPermanently, "/dashboard/vaults/"+url.PathEscape(c.Param("vault_id"))+"/theme-settings")
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

	// 管理员控制台。
	adminGroup := console.Group("/admin", h.requireAdmin)
	{
		adminGroup.GET("", h.adminUsersPage)
		adminGroup.POST("/users/:user_id/role", h.adminSetUserRole)
		adminGroup.POST("/users/:user_id/reset-password", h.adminResetPassword)
		adminGroup.POST("/users/:user_id/delete", h.adminDeleteUser)
		adminGroup.GET("/vaults", h.adminVaultsPage)
		adminGroup.GET("/vaults/:vault_id", h.adminVaultDetailPage)
		adminGroup.GET("/devices", h.adminDevicesPage)
		adminGroup.POST("/devices/:client_id/authorize", h.adminAuthorizeDevice)
		adminGroup.POST("/devices/:client_id/revoke", h.adminRevokeDevice)
		adminGroup.GET("/system", h.adminSystemPage)
		adminGroup.POST("/system", h.adminSaveSystem)
		adminGroup.GET("/data", h.adminDataPage)
		adminGroup.POST("/system/database", h.adminSaveDatabase)
		adminGroup.GET("/themes", h.adminThemesPage)
		adminGroup.POST("/themes/upload", h.adminThemeUpload)
		adminGroup.POST("/themes/scaffold", h.adminThemeScaffold)
		adminGroup.GET("/themes/:name/download", h.adminThemeDownload)
		adminGroup.POST("/themes/:name/delete", h.adminThemeDelete)
		adminGroup.POST("/themes/:name/files/save", h.adminThemeFileSave)
		adminGroup.GET("/console-themes", h.adminConsoleThemesPage)
		adminGroup.POST("/console-themes/upload", h.adminConsoleThemeUpload)
		adminGroup.POST("/console-themes/scaffold", h.adminConsoleThemeScaffold)
		adminGroup.GET("/console-themes/:name/download", h.adminConsoleThemeDownload)
		adminGroup.POST("/console-themes/:name/files/save", h.adminConsoleThemeFileSave)
		adminGroup.POST("/console-themes/:name/delete", h.adminConsoleThemeDelete)
		adminGroup.GET("/backups/:id/download", h.downloadBackup)
		adminGroup.POST("/backups/:id/delete", h.deleteBackup)
	}

	// 旧 /admin 路由重定向兼容，不再提供服务页面。
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

// 会话

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

// setSessionCookie 设置登录会话 cookie 与 CSRF cookie。
func (h *Handler) setSessionCookie(c *gin.Context, user *models.User) {
	token, expiresIn, err := auth.IssueToken(h.Cfg, *user)
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
	// CSRF token 与会话同生命周期。
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

// requireSession 要求已登录，并校验状态修改请求的 CSRF token。
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

func requestedWebLanguage(c *gin.Context) string {
	accept := c.GetHeader("Accept-Language")
	if accept == "" {
		return ""
	}
	// 遍历所有语言段，按 q 值选择客户端偏好最高的支持语言。
	// q=0 表示客户端明确不接受该语言，必须排除。
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
		// 取主语言子标签：zh-CN → zh, en-US → en。
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

// render 使用统一布局渲染控制台页面。page 为页面模板名。
func (h *Handler) render(c *gin.Context, status int, page, title string, activeGroup, activePage string, data any) {
	u := h.webUser(c)
	ld := layoutData{
		Page:        page,
		Title:       title,
		Username:    "",
		IsAdmin:     false,
		ShowSidebar: u != nil,
		ActiveGroup: activeGroup,
		ActivePage:  activePage,
	}
	if u != nil {
		ld.Username = u.Username
		ld.IsAdmin = u.Role == "admin"
		ld.ConsoleThemeName = h.selectedConsoleTheme(u.ID)
		ld.Language = h.userLang(c)
	}
	if token, err := c.Cookie(csrfCookie); err == nil {
		ld.CSRF = token
	}
	h.renderWithLayout(c, status, ld, data)
}

// renderWithLayout 渲染页面内容并把结果注入统一布局。
func (h *Handler) renderWithLayout(c *gin.Context, status int, ld layoutData, data any) {
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
	setPageHeaders(c)
	c.Status(status)
	_ = h.tpl.ExecuteTemplate(c.Writer, "layout", struct {
		Layout layoutData
		Data   any
	}{Layout: ld, Data: data})
}

func setPageHeaders(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "same-origin")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Frame-Options", "DENY")
	c.Header(
		"Content-Security-Policy",
		"default-src 'none'; connect-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: https:; "+
			"form-action 'self'; frame-ancestors 'none'; base-uri 'none'",
	)
}

func requestIsHTTPS(c *gin.Context) bool {
	return c.Request.TLS != nil ||
		strings.EqualFold(strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")), "https")
}

// 静态资源

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

// 登录与注册

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

// renderAuth 渲染登录/注册等无侧边栏页面。
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

// 网页文件操作

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
