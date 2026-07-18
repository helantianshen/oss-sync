// Package blog 提供公开分享页面和主题资源：
//
//   - GET /p/:share_id          单篇分享渲染
//   - GET /p/:share_id/*subpath 文件夹分享（subpath 空→目录树；命中文件→渲染）
//   - GET /themes/:theme/*      静态主题资源
package blog

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/filestore"
	"github.com/oss/oss-server/internal/markdown"
	"github.com/oss/oss-server/internal/models"
)

//go:embed templates/*.html
var templatesFS embed.FS

type Handler struct {
	DB  *gorm.DB
	Cfg *config.Config
	tpl *template.Template
}

func New(db *gorm.DB, cfg *config.Config) (*Handler, error) {
	tpl, err := template.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse blog templates: %w", err)
	}
	return &Handler{DB: db, Cfg: cfg, tpl: tpl}, nil
}

// Register 挂载无需登录的公开分享路由。
func (h *Handler) Register(r *gin.Engine) {
	r.GET("/p/:share_id", h.handleSingle)
	r.GET("/p/:share_id/*subpath", h.handleFolder)
	r.GET("/assets/:share_id", h.handleSharedAsset)
	r.GET("/themes/:theme/*filepath", h.handleThemeAsset)
}

// shareResolver 实现 markdown.LinkResolver。
// 索引按文件名匹配分享；同名时使用最近创建的分享。
type shareResolver struct {
	index map[string]string // basename(无 .md) -> share_id
}

var _ markdown.LinkResolver = (*shareResolver)(nil)

func (r *shareResolver) Resolve(linkText string) string {
	if r == nil {
		return ""
	}
	return r.index[linkText]
}

// buildResolver 构建当前 Vault 中可公开访问的双链索引。
func (h *Handler) buildResolver(userID uint, vaultID string) *shareResolver {
	type shareRow struct {
		ShareID    string
		TargetPath string
		IsFolder   bool
		CreatedAt  time.Time
	}
	var rows []shareRow
	h.DB.Model(&models.Share{}).
		Select("share_id", "target_path", "is_folder", "created_at").
		Where("user_id = ? AND vault_id = ?", userID, vaultID).
		Find(&rows)

	latest := map[string]shareRow{}
	for _, r := range rows {
		base := basenameNoExt(r.TargetPath)
		if base == "" {
			continue
		}
		if cur, ok := latest[base]; !ok || r.CreatedAt.After(cur.CreatedAt) {
			latest[base] = r
		}
	}

	// 文件夹内的文章没有独立 share_id，双链统一指向文件夹分享。
	for _, r := range rows {
		if !r.IsFolder {
			continue
		}
		var files []models.File
		prefix := strings.TrimSuffix(r.TargetPath, "/") + "/"
		h.DB.Where(
			"user_id = ? AND vault_id = ? AND path LIKE ? AND is_deleted = ? AND type = ?",
			userID, vaultID, prefix+"%", false, "markdown",
		).Find(&files)
		for _, f := range files {
			base := basenameNoExt(f.Path)
			if base == "" {
				continue
			}
			if _, ok := latest[base]; !ok {
				latest[base] = r
			}
		}
	}

	idx := make(map[string]string, len(latest))
	for base, r := range latest {
		idx[base] = r.ShareID
	}
	return &shareResolver{index: idx}
}

func basenameNoExt(p string) string {
	base := path.Base(p)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

type renderParams struct {
	Title         string
	ThemeName     string
	ThemeConfigJS template.JS
	CustomHeader  template.HTML
	CustomFooter  template.HTML
	ContentHTML   template.HTML
	IsFolder      bool
	FolderTitle   string
	FooterNotice  template.HTML
}

// loadVaultSettings 优先读取 Vault 配置，并兼容旧版用户级配置。
func (h *Handler) loadVaultSettings(userID uint, vaultID string) (*models.VaultSetting, error) {
	var vs models.VaultSetting
	if err := h.DB.Where("vault_id = ?", vaultID).First(&vs).Error; err == nil {
		if vs.ThemeName == "" {
			vs.ThemeName = "default"
		}
		return &vs, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	var us models.UserSetting
	if err := h.DB.Where("user_id = ?", userID).First(&us).Error; err != nil {
		return &models.VaultSetting{VaultID: vaultID, ThemeName: "default"}, nil
	}
	return &models.VaultSetting{
		VaultID:           vaultID,
		ThemeName:         us.ThemeName,
		ThemeConfig:       us.ThemeConfig,
		CustomHeader:      us.CustomHeader,
		CustomFooter:      us.CustomFooter,
		KeepDirectoryTree: us.KeepDirectoryTree,
	}, nil
}

func (h *Handler) renderTemplate(c *gin.Context, p renderParams) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := h.tpl.ExecuteTemplate(c.Writer, "base.html", p); err != nil {
		_ = err
	}
}

func (h *Handler) renderRemoved(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Status(http.StatusNotFound)
	_ = h.tpl.ExecuteTemplate(c.Writer, "removed.html", nil)
}

func (h *Handler) handleSingle(c *gin.Context) {
	shareID := c.Param("share_id")
	var share models.Share
	if err := h.DB.Where("share_id = ?", shareID).First(&share).Error; err != nil {
		h.renderRemoved(c)
		return
	}
	if share.IsFolder {
		c.Redirect(http.StatusFound, "/p/"+shareID+"/")
		return
	}

	_ = h.DB.Model(&models.Share{}).Where("share_id = ?", shareID).
		UpdateColumn("views", gorm.Expr("views + 1")).Error

	var f models.File
	if err := h.DB.Where(
		"user_id = ? AND vault_id = ? AND path = ? AND is_deleted = ?",
		share.UserID, share.VaultID, share.TargetPath, false,
	).First(&f).Error; err != nil {
		h.renderRemoved(c)
		return
	}
	abs := filestore.DiskPath(h.Cfg.Storage.DataDir, f)
	raw, err := readFileUTF8(abs)
	if err != nil {
		h.renderRemoved(c)
		return
	}

	resolver := h.buildResolver(share.UserID, share.VaultID)
	html, err := markdown.RenderMarkdownWithAssets(resolver, blogAssetResolver{shareID: share.ShareID}, raw)
	if err != nil {
		c.String(http.StatusInternalServerError, "render failed: %v", err)
		return
	}

	us, _ := h.loadVaultSettings(share.UserID, share.VaultID)
	themeConfigJSON, _ := json.Marshal(us.ThemeConfig)
	params := renderParams{
		Title:         basenameNoExt(f.Path) + " · OSS",
		ThemeName:     us.ThemeName,
		ThemeConfigJS: template.JS(themeConfigJSON),
		CustomHeader:  template.HTML(us.CustomHeader),
		CustomFooter:  template.HTML(us.CustomFooter),
		ContentHTML:   template.HTML(html),
	}
	h.renderTemplate(c, params)
}

func (h *Handler) handleFolder(c *gin.Context) {
	shareID := c.Param("share_id")
	subpath := strings.TrimPrefix(c.Param("subpath"), "/")
	subpath = strings.TrimSuffix(subpath, "/")

	var share models.Share
	if err := h.DB.Where("share_id = ?", shareID).First(&share).Error; err != nil {
		h.renderRemoved(c)
		return
	}
	if !share.IsFolder {
		c.Redirect(http.StatusFound, "/p/"+shareID)
		return
	}

	_ = h.DB.Model(&models.Share{}).Where("share_id = ?", shareID).
		UpdateColumn("views", gorm.Expr("views + 1")).Error

	prefix := strings.TrimSuffix(share.TargetPath, "/") + "/"
	var files []models.File
	h.DB.Where(
		"user_id = ? AND vault_id = ? AND path LIKE ? AND is_deleted = ? AND type = ?",
		share.UserID, share.VaultID, prefix+"%", false, "markdown",
	).
		Order("path asc").
		Find(&files)

	if subpath == "" {
		h.renderFolderTree(c, share, files)
		return
	}

	targetPath := prefix + subpath
	for _, f := range files {
		if f.Path == targetPath {
			h.renderFolderFile(c, share, f)
			return
		}
	}
	h.renderRemoved(c)
}

func (h *Handler) renderFolderTree(c *gin.Context, share models.Share, files []models.File) {
	var b strings.Builder
	b.WriteString("<ul class=\"oss-tree-list\">")
	for _, f := range files {
		rel := strings.TrimPrefix(f.Path, strings.TrimSuffix(share.TargetPath, "/")+"/")
		href := "/p/" + share.ShareID + "/" + rel
		title := strings.TrimSuffix(path.Base(f.Path), filepath.Ext(f.Path))
		b.WriteString(fmt.Sprintf(`<li><a href="%s">%s</a></li>`, href, htmlEscape(title)))
	}
	b.WriteString("</ul>")

	us, _ := h.loadVaultSettings(share.UserID, share.VaultID)
	themeConfigJSON, _ := json.Marshal(us.ThemeConfig)
	params := renderParams{
		Title:         "Folder · " + share.TargetPath,
		ThemeName:     us.ThemeName,
		ThemeConfigJS: template.JS(themeConfigJSON),
		CustomHeader:  template.HTML(us.CustomHeader),
		CustomFooter:  template.HTML(us.CustomFooter),
		IsFolder:      true,
		FolderTitle:   share.TargetPath,
		ContentHTML:   template.HTML(b.String()),
	}
	h.renderTemplate(c, params)
}

func (h *Handler) renderFolderFile(c *gin.Context, share models.Share, f models.File) {
	abs := filestore.DiskPath(h.Cfg.Storage.DataDir, f)
	raw, err := readFileUTF8(abs)
	if err != nil {
		h.renderRemoved(c)
		return
	}
	resolver := h.buildResolver(share.UserID, share.VaultID)
	html, err := markdown.RenderMarkdownWithAssets(resolver, blogAssetResolver{shareID: share.ShareID}, raw)
	if err != nil {
		c.String(http.StatusInternalServerError, "render failed: %v", err)
		return
	}

	us, _ := h.loadVaultSettings(share.UserID, share.VaultID)
	themeConfigJSON, _ := json.Marshal(us.ThemeConfig)
	params := renderParams{
		Title:         basenameNoExt(f.Path) + " · " + share.TargetPath,
		ThemeName:     us.ThemeName,
		ThemeConfigJS: template.JS(themeConfigJSON),
		CustomHeader:  template.HTML(us.CustomHeader),
		CustomFooter:  template.HTML(us.CustomFooter),
		ContentHTML:   template.HTML(html),
	}
	h.renderTemplate(c, params)
}

func (h *Handler) handleThemeAsset(c *gin.Context) {
	theme := c.Param("theme")
	fp := c.Param("filepath")
	if theme == "" || fp == "" || strings.Contains(theme, "..") || strings.Contains(fp, "..") {
		c.String(http.StatusBadRequest, "invalid theme or path")
		return
	}
	fp = strings.TrimPrefix(fp, "/")
	if theme == "default" && h.serveDefaultTheme(c, fp) {
		return
	}
	abs := filepath.Join(h.Cfg.Storage.DataDir, "themes", theme, fp)
	c.File(abs)
}

func readFileUTF8(abs string) (string, error) {
	b, err := readFile(abs)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func readFile(abs string) ([]byte, error) {
	return osReadFile(abs)
}

var osReadFile = func(p string) ([]byte, error) {
	return os.ReadFile(p)
}

func htmlEscape(s string) string {
	return template.HTMLEscapeString(s)
}
