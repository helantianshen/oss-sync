package webui

import (
	"bytes"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/helantianshen/oss-sync/internal/blog"
	"github.com/helantianshen/oss-sync/internal/consoletheme"
	"github.com/helantianshen/oss-sync/internal/markdown"
	"github.com/helantianshen/oss-sync/internal/models"
	"github.com/helantianshen/oss-sync/internal/serverplugin"
)

type adminPluginsData struct {
	Plugins   []serverplugin.PluginInfo
	GuideHTML template.HTML
	// GuideSource 是插件指南的 Markdown 原文，注入 script[type=text/plain] 供复制使用
	GuideSource string
	Error       string
	Saved       bool
	EditID      string
	EditFiles   []serverplugin.EditablePluginFile
	// Updates 是 manifest 声明更新源且有新版本的插件，key 为插件 ID
	Updates map[string]serverplugin.PluginUpdateInfo
	// Updated 与 UpdatedVersion 描述刚完成的一次更新，用于成功提示
	Updated        string
	UpdatedVersion string
}

func (h *Handler) adminPluginPage(c *gin.Context) {
	if h.pluginManager == nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	content, err := h.pluginManager.RenderAdminPage(c.Request.Context(), c.Param("id"), c.Param("slug"))
	if err != nil {
		c.Status(http.StatusBadGateway)
		return
	}
	h.renderPluginAdminContent(c, c.Param("id"), c.Param("slug"), content)
}

func (h *Handler) renderPluginAdminContent(c *gin.Context, pluginID, slug, content string) {
	if pluginReturnsDocument(content) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(content))
		return
	}
	u := h.webUser(c)
	pages := h.pluginManager.AdminPages()
	ld := layoutData{
		Page:             "admin-plugin-page",
		Title:            pluginAdminPageTitle(pages, pluginID, slug),
		Username:         u.Username,
		IsAdmin:          u.Role == "admin",
		ShowSidebar:      true,
		ActiveGroup:      "admin",
		ActivePage:       "admin-plugin-page",
		ActivePluginID:   pluginID,
		ConsoleThemeName: h.selectedConsoleTheme(u.ID),
		Language:         h.userLang(c),
		NavVaults:        h.accessibleVaults(u),
		PluginAdminPages: pages,
		ContentHTML:      template.HTML(content),
	}
	if selected, disabledTheme := h.selectedConsoleThemeState(u.ID); disabledTheme != "" {
		ld.ConsoleThemeName = selected
		ld.Flash = h.t(c, "admin.plugin_theme_disabled", disabledTheme)
		ld.FlashKind = "error"
	}
	h.setPluginNavigationForUser(&ld, u)
	if token, err := c.Cookie(csrfCookie); err == nil {
		ld.CSRF = token
	}
	h.renderWithLayout(c, http.StatusOK, ld, nil)
}

// pluginReturnsDocument 判断插件是否接管完整 HTML 文档
func pluginReturnsDocument(content string) bool {
	trimmed := strings.TrimPrefix(strings.TrimSpace(content), "\ufeff")
	trimmed = strings.ToLower(trimmed)
	return strings.HasPrefix(trimmed, "<!doctype html") || strings.HasPrefix(trimmed, "<html")
}

func pluginAdminPageTitle(pages []serverplugin.PluginAdminPage, pluginID, slug string) string {
	for _, page := range pages {
		if page.PluginID == pluginID && page.Slug == slug && page.Label != "" {
			return page.Label
		}
	}
	return slug
}

func (h *Handler) adminPluginsPage(c *gin.Context) {
	d := adminPluginsData{Error: c.Query("error"), Saved: c.Query("saved") == "1"}
	if updated := strings.TrimSpace(c.Query("updated")); updated != "" {
		d.Updated = updated
		d.UpdatedVersion = strings.TrimSpace(c.Query("latest"))
	}
	guideName := "assets/plugin-guide.md"
	if h.userLang(c) == "zh" {
		guideName = "assets/plugin-guide.zh.md"
	}
	if source, err := webFS.ReadFile(guideName); err == nil {
		if guide, renderErr := markdown.RenderMarkdown(nil, string(source)); renderErr == nil {
			d.GuideHTML = template.HTML(guide)
		}
		d.GuideSource = string(source)
	}
	if h.pluginManager == nil {
		d.Error = h.t(c, "admin.plugins_unavailable")
		h.render(c, http.StatusServiceUnavailable, "admin-plugins", h.t(c, "page.admin_plugins"), "admin", "admin-plugins", d)
		return
	}
	plugins, err := h.pluginManager.List()
	if err != nil {
		d.Error = h.t(c, "admin.plugins_load_failed")
		h.render(c, http.StatusInternalServerError, "admin-plugins", h.t(c, "page.admin_plugins"), "admin", "admin-plugins", d)
		return
	}
	d.Plugins = plugins
	d.EditID = strings.TrimSpace(c.Query("edit"))
	if d.EditID != "" {
		d.EditFiles, err = h.pluginManager.EditableFiles(d.EditID)
		if err != nil {
			d.Error = err.Error()
			d.EditID = ""
		}
	}
	// 查询参数可直接带入一次检查结果，无需每次渲染都请求作者接口
	if available := strings.TrimSpace(c.Query("update")); available != "" {
		latest := strings.TrimSpace(c.Query("latest"))
		d.Updates = map[string]serverplugin.PluginUpdateInfo{
			available: {PluginID: available, Latest: latest, HasUpdate: latest != "", UpdatableHere: true},
		}
	}
	h.render(c, http.StatusOK, "admin-plugins", h.t(c, "page.admin_plugins"), "admin", "admin-plugins", d)
}

func (h *Handler) adminPluginUpload(c *gin.Context) {
	if h.pluginManager == nil {
		h.redirectPluginError(c, "admin.plugins_unavailable")
		return
	}
	file, err := c.FormFile("file")
	if err != nil {
		h.redirectPluginError(c, "admin.plugin_file_required")
		return
	}
	if !strings.HasSuffix(strings.ToLower(file.Filename), ".zip") {
		h.redirectPluginError(c, "admin.plugin_zip_required")
		return
	}
	reader, err := file.Open()
	if err != nil {
		h.redirectPluginError(c, "admin.plugin_read_failed")
		return
	}
	defer reader.Close()
	content, err := io.ReadAll(io.LimitReader(reader, serverplugin.MaxArchiveBytes+1))
	if err != nil {
		h.redirectPluginError(c, "admin.plugin_read_failed")
		return
	}
	if len(content) > serverplugin.MaxArchiveBytes {
		h.redirectPluginError(c, "admin.plugin_too_large")
		return
	}
	info, err := h.pluginManager.Install(c.Request.Context(), bytes.NewReader(content), int64(len(content)))
	if err != nil {
		if errors.Is(err, serverplugin.ErrPluginExists) {
			info, err = h.pluginManager.Upgrade(c.Request.Context(), bytes.NewReader(content), int64(len(content)))
			if err != nil {
				h.redirectPluginError(c, "admin.plugin_install_failed")
				return
			}
		} else {
			h.redirectPluginError(c, "admin.plugin_install_failed")
			return
		}
	}
	if err := h.pluginManager.Enable(c.Request.Context(), info.ID); err != nil {
		h.redirectPluginError(c, "admin.plugin_enable_failed")
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/admin/plugins?saved=1")
}

func (h *Handler) adminPluginFileSave(c *gin.Context) {
	if h.pluginManager == nil {
		h.redirectPluginError(c, "admin.plugins_unavailable")
		return
	}
	if err := h.pluginManager.SaveTextFile(c.Request.Context(), c.Param("id"), c.PostForm("path"), c.PostForm("content")); err != nil {
		c.Redirect(http.StatusSeeOther, "/dashboard/admin/plugins?edit="+url.QueryEscape(c.Param("id"))+"&error="+url.QueryEscape(err.Error()))
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/admin/plugins?edit="+url.QueryEscape(c.Param("id"))+"&saved=1")
}

func (h *Handler) adminPluginEnable(c *gin.Context) {
	if h.pluginManager == nil {
		h.redirectPluginError(c, "admin.plugins_unavailable")
		return
	}
	if err := h.pluginManager.Enable(c.Request.Context(), c.Param("id")); err != nil {
		h.redirectPluginError(c, "admin.plugin_enable_failed")
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/admin/plugins?saved=1")
}

func (h *Handler) adminPluginDisable(c *gin.Context) {
	if h.pluginManager == nil {
		h.redirectPluginError(c, "admin.plugins_unavailable")
		return
	}
	if err := h.pluginManager.Disable(c.Request.Context(), c.Param("id")); err != nil {
		h.redirectPluginError(c, "admin.plugin_disable_failed")
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/admin/plugins?saved=1")
}

func (h *Handler) adminPluginDelete(c *gin.Context) {
	if h.pluginManager == nil {
		h.redirectPluginError(c, "admin.plugins_unavailable")
		return
	}
	var links []models.ServerPluginAssociation
	if err := h.DB.Where("plugin_id = ?", c.Param("id")).Find(&links).Error; err != nil {
		h.redirectPluginError(c, "admin.plugin_delete_failed")
		return
	}
	for _, link := range links {
		if link.Kind == "blog_theme" {
			if _, err := blog.DeleteTheme(h.DB, h.Cfg.Storage.DataDir, link.TargetID); err != nil {
				h.redirectPluginError(c, "admin.plugin_delete_association_failed")
				return
			}
		}
		if link.Kind == "console_theme" {
			if err := consoletheme.Delete(h.Cfg.Storage.DataDir, link.TargetID); err != nil && !errors.Is(err, consoletheme.ErrNotFound) {
				h.redirectPluginError(c, "admin.plugin_delete_association_failed")
				return
			}
		}
	}
	if err := h.pluginManager.Delete(c.Param("id")); err != nil {
		h.redirectPluginError(c, "admin.plugin_delete_failed")
		return
	}
	if err := h.DB.Where("plugin_id = ?", c.Param("id")).Delete(&models.ServerPluginAssociation{}).Error; err != nil {
		h.redirectPluginError(c, "admin.plugin_delete_failed")
		return
	}
	c.Redirect(http.StatusSeeOther, "/dashboard/admin/plugins?saved=1")
}

// adminPluginGuideMarkdown 按当前语言导出插件指南原文，供作者下载后离线查阅
func (h *Handler) adminPluginGuideMarkdown(c *gin.Context) {
	guideName := "assets/plugin-guide.md"
	if h.userLang(c) == "zh" {
		guideName = "assets/plugin-guide.zh.md"
	}
	source, err := webFS.ReadFile(guideName)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="plugin-guide.md"`)
	c.Data(http.StatusOK, "text/markdown; charset=utf-8", source)
}

func (h *Handler) adminPluginUpdateCheck(c *gin.Context) {
	if h.pluginManager == nil {
		h.redirectPluginError(c, "admin.plugins_unavailable")
		return
	}
	updates, err := h.pluginManager.CheckPluginUpdates(c.Request.Context())
	if err != nil {
		h.redirectPluginError(c, "admin.plugin_update_check_failed")
		return
	}
	for _, update := range updates {
		if update.PluginID != c.Param("id") {
			continue
		}
		if update.CheckFailed {
			h.redirectPluginError(c, "admin.plugin_update_check_failed")
			return
		}
		if !update.HasUpdate {
			h.redirectPluginError(c, "admin.plugin_up_to_date")
			return
		}
		if !update.UpdatableHere {
			h.redirectPluginMessage(c, h.t(c, "admin.plugin_update_manual", update.Latest))
			return
		}
		c.Redirect(http.StatusSeeOther, "/dashboard/admin/plugins?update="+
			url.QueryEscape(update.PluginID)+"&latest="+url.QueryEscape(update.Latest))
		return
	}
	h.redirectPluginError(c, "admin.plugin_no_update_source")
}

func (h *Handler) adminPluginUpdate(c *gin.Context) {
	if h.pluginManager == nil {
		h.redirectPluginError(c, "admin.plugins_unavailable")
		return
	}
	updates, err := h.pluginManager.CheckPluginUpdates(c.Request.Context())
	if err != nil {
		h.redirectPluginError(c, "admin.plugin_update_check_failed")
		return
	}
	for _, update := range updates {
		if update.PluginID != c.Param("id") {
			continue
		}
		if update.CheckFailed {
			h.redirectPluginError(c, "admin.plugin_update_check_failed")
			return
		}
		if !update.UpdatableHere {
			continue
		}
		if _, updateErr := h.pluginManager.PluginUpdateFromURL(c.Request.Context(), update.PluginID, update.Latest, update.DownloadURL); updateErr != nil {
			h.redirectPluginError(c, "admin.plugin_update_failed")
			return
		}
		c.Redirect(http.StatusSeeOther, "/dashboard/admin/plugins?updated="+url.QueryEscape(update.PluginID)+"&latest="+url.QueryEscape(update.Latest))
		return
	}
	h.redirectPluginError(c, "admin.plugin_no_update_source")
}

func (h *Handler) redirectPluginError(c *gin.Context, key string) {
	c.Redirect(http.StatusSeeOther, "/dashboard/admin/plugins?error="+url.QueryEscape(h.t(c, key)))
}

// redirectPluginMessage 直接重定向到已在调用处完成插值的提示文本
func (h *Handler) redirectPluginMessage(c *gin.Context, message string) {
	c.Redirect(http.StatusSeeOther, "/dashboard/admin/plugins?error="+url.QueryEscape(message))
}
