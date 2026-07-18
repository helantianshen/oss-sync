// Package blog 实现 OSS 轻博客渲染：
//
//   - GET /p/:share_id          单篇分享渲染
//   - GET /p/:share_id/*subpath 文件夹分享（subpath 空→目录树；命中文件→渲染）
//   - GET /themes/:theme/*      静态主题资源
//
// 决策落地：
//
//	决策 3：双链 [[...]] 全局匹配当前用户 shares，仅已分享渲染为链接。
//	        文件夹分享路由预先把文件夹下所有文件视为"可被双链命中"，
//	        合并进全局查找集合，构建 O(1) 索引交给 Goldmark 扩展。
//	决策 4：CustomHeader/CustomFooter 用 template.HTML 原样输出。
//	决策 5：ThemeConfig 用 template.JS 注入。
//	决策 6.4：分享文件已删除/不存在返 removed.html 友好提示而非裸 404。
package blog

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/markdown"
	"github.com/oss/oss-server/internal/models"
)

//go:embed templates/*.html
var templatesFS embed.FS

// Handler 持有博客路由依赖。
type Handler struct {
	DB  *gorm.DB
	Cfg *config.Config
	tpl *template.Template
}

// New 创建 Handler 并预编译模板。
func New(db *gorm.DB, cfg *config.Config) (*Handler, error) {
	tpl, err := template.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse blog templates: %w", err)
	}
	return &Handler{DB: db, Cfg: cfg, tpl: tpl}, nil
}

// Register 挂载博客路由。注意：博客路由不需要 JWT，对公网开放。
// share_id 自身作为访问凭据（短链）。
func (h *Handler) Register(r *gin.Engine) {
	r.GET("/p/:share_id", h.handleSingle)
	r.GET("/p/:share_id/*subpath", h.handleFolder)
	r.GET("/assets/:share_id", h.handleSharedAsset)
	r.GET("/themes/:theme/*filepath", h.handleThemeAsset)
}

// --- Link Resolver（决策 3 全局匹配） ---

// shareResolver 实现 markdown.LinkResolver。
// 决策 3：在渲染前一次性加载该用户的所有 shares + 文件夹分享覆盖的文件路径，
// 构建 文件名(无后缀) → share_id 的内存索引，O(1) 查询。
// 同名歧义取 CreatedAt 最近更新的 share_id。
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

// buildResolver 为某用户构建双链解析索引。
// 决策 3 末尾：文件夹分享覆盖其下所有文件——把文件夹下所有文件
// 视为"可被双链命中"，合并进全局查找集合。
func (h *Handler) buildResolver(userID uint) *shareResolver {
	type shareRow struct {
		ShareID    string
		TargetPath string
		IsFolder   bool
		CreatedAt  time.Time
	}
	var rows []shareRow
	h.DB.Model(&models.Share{}).
		Select("share_id", "target_path", "is_folder", "created_at").
		Where("user_id = ?", userID).
		Find(&rows)

	// 同名歧义：保留 CreatedAt 最大的
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

	// 文件夹分享：把文件夹下所有 markdown 文件视为"可被双链命中"，
	// 但它们没有独立 share_id——双链应指向该文件夹分享 + 锚点 subpath。
	// 决策 3：文件夹分享覆盖范围内文件视为可命中。
	// 我们把它们也加入索引，share_id 用文件夹分享 + ?sub= 路径形式。
	// 但简化：本期 Phase 4 仅对单篇 share_id 渲染链接，文件夹内子文件
	// 的双链命中也指向文件夹 share_id（不带锚点），由前端跳转后用 subpath
	// 查找。后续若需要更精细，再扩展 subpath。
	for _, r := range rows {
		if !r.IsFolder {
			continue
		}
		// 列出文件夹下所有 markdown 文件
		var files []models.File
		prefix := strings.TrimSuffix(r.TargetPath, "/") + "/"
		h.DB.Where("user_id = ? AND path LIKE ? AND is_deleted = ? AND type = ?",
			userID, prefix+"%", false, "markdown").Find(&files)
		for _, f := range files {
			base := basenameNoExt(f.Path)
			if base == "" {
				continue
			}
			// 单篇分享优先，文件夹分享兜底
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

// --- 渲染入口 ---

// renderParams 是模板渲染用的参数集。
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

// loadUserSettings 取用户配置（含主题/注入字段）。
func (h *Handler) loadUserSettings(userID uint) (*models.UserSetting, error) {
	var us models.UserSetting
	if err := h.DB.Where("user_id = ?", userID).First(&us).Error; err != nil {
		// 用户尚未建 UserSetting 行：返回默认配置，不阻断渲染
		return &models.UserSetting{
			ThemeName: "default",
		}, nil
	}
	return &us, nil
}

// renderTemplate 执行 base.html，注入所有决策 4/5 字段。
func (h *Handler) renderTemplate(c *gin.Context, p renderParams) {
	// 决策 5：ThemeConfig 序列化为 JSON，再以 template.JS 注入。
	// 模板侧 {{.ThemeConfigJS}} 不会被 html/template 转义。
	var b []byte
	if p.ThemeConfigJS == "" {
		b = []byte("null")
	} else {
		b = []byte(p.ThemeConfigJS)
	}
	// 这里 ThemeConfigJS 字段已是 template.JS；构造时调用方需先把 JSON marshal
	// 成 string 再 cast 为 template.JS。
	_ = b

	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := h.tpl.ExecuteTemplate(c.Writer, "base.html", p); err != nil {
		// 模板渲染中途失败，已部分写出，只能记日志
		_ = err
	}
}

// renderRemoved 渲染"已移除"友好页面（决策 6.4）。
func (h *Handler) renderRemoved(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Status(http.StatusNotFound)
	_ = h.tpl.ExecuteTemplate(c.Writer, "removed.html", nil)
}

// --- 路由：单篇 ---

func (h *Handler) handleSingle(c *gin.Context) {
	shareID := c.Param("share_id")
	var share models.Share
	if err := h.DB.Where("share_id = ?", shareID).First(&share).Error; err != nil {
		h.renderRemoved(c)
		return
	}
	if share.IsFolder {
		// 文件夹分享走 /p/:share_id/*subpath；如果用户直接访问单篇路由，
		// 重定向到无 subpath 的文件夹分享根。
		c.Redirect(http.StatusFound, "/p/"+shareID+"/")
		return
	}

	// 浏览量 +1
	_ = h.DB.Model(&models.Share{}).Where("share_id = ?", shareID).
		UpdateColumn("views", gorm.Expr("views + 1")).Error

	// 取文件
	var f models.File
	if err := h.DB.Where("user_id = ? AND path = ? AND is_deleted = ?",
		share.UserID, share.TargetPath, false).First(&f).Error; err != nil {
		h.renderRemoved(c)
		return
	}
	abs := filepath.Join(h.Cfg.Storage.DataDir, fmt.Sprintf("%d", share.UserID), f.Path)
	raw, err := readFileUTF8(abs)
	if err != nil {
		h.renderRemoved(c)
		return
	}

	// 决策 3：构建双链解析索引
	resolver := h.buildResolver(share.UserID)
	html, err := markdown.RenderMarkdownWithAssets(resolver, blogAssetResolver{shareID: share.ShareID}, raw)
	if err != nil {
		c.String(http.StatusInternalServerError, "render failed: %v", err)
		return
	}

	us, _ := h.loadUserSettings(share.UserID)
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

// --- 路由：文件夹分享 ---

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
		// 单篇分享访问了 subpath——重定向到单篇路由
		c.Redirect(http.StatusFound, "/p/"+shareID)
		return
	}

	// 浏览量 +1
	_ = h.DB.Model(&models.Share{}).Where("share_id = ?", shareID).
		UpdateColumn("views", gorm.Expr("views + 1")).Error

	// 列出文件夹下所有未删 markdown 文件
	prefix := strings.TrimSuffix(share.TargetPath, "/") + "/"
	var files []models.File
	h.DB.Where("user_id = ? AND path LIKE ? AND is_deleted = ? AND type = ?",
		share.UserID, prefix+"%", false, "markdown").
		Order("path asc").
		Find(&files)

	// subpath 空 → 渲染目录树
	if subpath == "" {
		h.renderFolderTree(c, share, files)
		return
	}

	// subpath 命中具体文件 → 渲染
	targetPath := prefix + subpath
	for _, f := range files {
		if f.Path == targetPath {
			h.renderFolderFile(c, share, f, files)
			return
		}
	}
	// 未命中
	h.renderRemoved(c)
}

// renderFolderTree 渲染目录树（左侧导航 + 简介首页）。
// 简化版：直接生成 <ul><li><a>...</a></li></ul> 列表，主题侧用 CSS 美化。
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

	us, _ := h.loadUserSettings(share.UserID)
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

// renderFolderFile 渲染文件夹内某文件，双链命中可指向文件夹内其它文件。
func (h *Handler) renderFolderFile(c *gin.Context, share models.Share, f models.File, allFiles []models.File) {
	abs := filepath.Join(h.Cfg.Storage.DataDir, fmt.Sprintf("%d", share.UserID), f.Path)
	raw, err := readFileUTF8(abs)
	if err != nil {
		h.renderRemoved(c)
		return
	}
	// 决策 3：文件夹分享下双链命中范围 = 文件夹下所有文件 + 用户全局 shares。
	// buildResolver 已经把文件夹分享覆盖的文件合并进索引。
	resolver := h.buildResolver(share.UserID)
	html, err := markdown.RenderMarkdownWithAssets(resolver, blogAssetResolver{shareID: share.ShareID}, raw)
	if err != nil {
		c.String(http.StatusInternalServerError, "render failed: %v", err)
		return
	}

	us, _ := h.loadUserSettings(share.UserID)
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

// --- 静态主题资源 ---

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
	// 主题目录约定在 data/themes/{theme}/{file}
	abs := filepath.Join(h.Cfg.Storage.DataDir, "themes", theme, fp)
	c.File(abs)
}

// --- 工具 ---

func readFileUTF8(abs string) (string, error) {
	b, err := readFile(abs)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// readFile 单独抽出便于测试 mock。
func readFile(abs string) ([]byte, error) {
	// 用 os.ReadFile：MVP 阶段博客 markdown 文件不会太大（KB 级），
	// 不需要流式。大文件附件走 /api/sync/download 流式。
	return osReadFile(abs)
}

// osReadFile 包一层，便于单测替换。
var osReadFile = func(p string) ([]byte, error) {
	return os.ReadFile(p)
}

func htmlEscape(s string) string {
	// 模板渲染正文时由 goldmark 自身转义；目录树这里手写一次
	return template.HTMLEscapeString(s)
}

// 排序辅助（保留扩展用，目前 SQL 已 order by）
var _ = sort.Strings
