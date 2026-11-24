package blog

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/consoletheme"
	"github.com/helantianshen/oss-sync/internal/filestore"
	"github.com/helantianshen/oss-sync/internal/models"
	"github.com/helantianshen/oss-sync/internal/settingspolicy"
)

// BlogThemeConfig 是公开博客通用展示字段的结构化配置
type BlogThemeConfig struct {
	LogoURL         string       `json:"logo_url"`
	LogoSize        int          `json:"logo_size"`
	LogoShape       string       `json:"logo_shape"`
	BlogName        string       `json:"blog_name"`
	Description     string       `json:"description"`
	Buttons         []BlogButton `json:"buttons"`
	BannerURL       string       `json:"banner_url"`
	MobileBannerURL string       `json:"mobile_banner_url"`
}

// BlogButton 表示博客页可展示的自定义链接
type BlogButton struct {
	Label    string `json:"label"`
	URL      string `json:"url"`
	IconURL  string `json:"icon_url"`
	Position int    `json:"position"`
}

// ParseBlogThemeConfig 从 Vault 配置解析公开博客通用展示字段
func ParseBlogThemeConfig(themeConfig map[string]any) BlogThemeConfig {
	cfg := BlogThemeConfig{}

	if themeConfig == nil {
		return cfg
	}
	if v, ok := themeConfig["logo_size"].(string); ok {
		cfg.LogoSize = parseBlogLogoSize(v)
	} else if v, ok := themeConfig["logo_size"].(float64); ok {
		cfg.LogoSize = parseBlogLogoSize(fmt.Sprintf("%d", int(v)))
	} else if v, ok := themeConfig["logo_size"].(int); ok {
		cfg.LogoSize = parseBlogLogoSize(fmt.Sprintf("%d", v))
	}
	if v, ok := themeConfig["logo_url"].(string); ok {
		cfg.LogoURL = v
	}
	if v, ok := themeConfig["logo_shape"].(string); ok && (v == "square" || v == "circle") {
		cfg.LogoShape = v
	}
	if v, ok := themeConfig["blog_name"].(string); ok {
		cfg.BlogName = v
	}
	if v, ok := themeConfig["description"].(string); ok {
		cfg.Description = v
	}
	if v, ok := themeConfig["banner_url"].(string); ok {
		cfg.BannerURL = v
	}
	if v, ok := themeConfig["mobile_banner_url"].(string); ok {
		cfg.MobileBannerURL = v
	}
	if rawButtons, ok := themeConfig["buttons"].([]any); ok {
		for _, raw := range rawButtons {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			btn := BlogButton{}
			if v, ok := m["label"].(string); ok {
				btn.Label = v
			}
			if v, ok := m["url"].(string); ok {
				btn.URL = v
			}
			if v, ok := m["icon_url"].(string); ok {
				btn.IconURL = v
			}
			if v, ok := m["position"].(float64); ok {
				btn.Position = int(v)
			} else if v, ok := m["position"].(int); ok {
				btn.Position = v
			}
			if btn.Label != "" && btn.URL != "" {
				cfg.Buttons = append(cfg.Buttons, btn)
			}
		}
	}
	return cfg
}

func parseBlogLogoSize(raw string) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	if value < 10 || value > 192 {
		return 0
	}
	return value
}

// HomePost 首页文章条目
type HomePost struct {
	Title   string
	Summary string
	URL     string
	Date    string
	Time    time.Time
	// 自定义主题使用的文章元数据
	Category  string
	Tags      []string
	CoverURL  string
	WordCount int
}

// PublicBlog 描述未登录首页上可发现的一个 Vault
type PublicBlog struct {
	Name        string
	Description string
	LogoURL     string
	LogoShape   string
	URL         string
}

type publicHomeData struct {
	Blogs []PublicBlog
	// ThemeName 是公开首页应用的内置控制台主题；由管理员设置
	ThemeName string
}

// handleHome 列出显式开启公开博客的全部 Vault
func (h *Handler) handleHome(c *gin.Context) {
	var settings []models.VaultSetting
	if err := h.DB.Where("is_public_blog = ?", true).Order("updated_at desc").Find(&settings).Error; err != nil {
		c.String(http.StatusInternalServerError, "load public blogs failed")
		return
	}
	blogs := make([]PublicBlog, 0, len(settings))
	for _, setting := range settings {
		var vault models.Vault
		if err := h.DB.Where("id = ?", setting.VaultID).First(&vault).Error; err != nil {
			continue
		}
		config := h.publicThemeConfig(setting.VaultID, setting.ThemeName, setting.ThemeConfig)
		cfg := ParseBlogThemeConfig(config)
		description := cfg.Description
		if description == "" {
			description = vault.Description
		}
		blogs = append(blogs, PublicBlog{
			Name:        blogTitle(cfg, vault.Name),
			Description: description,
			LogoURL:     cfg.LogoURL,
			LogoShape:   cfg.LogoShape,
			URL:         "/b/" + vault.ID,
		})
	}
	// 公开首页主题：管理员在系统设置中指定的内置控制台主题
	h.renderPublicHome(c, publicHomeData{Blogs: blogs, ThemeName: h.publicHomeTheme()})
}

// publicHomeTheme 读取管理员为公开首页设置的内置控制台主题，未设置或失效时回退 default
func (h *Handler) publicHomeTheme() string {
	var setting models.SystemSetting
	if err := h.DB.Where("id = 1").First(&setting).Error; err != nil {
		return consoletheme.BuiltinDefault
	}
	if setting.PublicHomeTheme != "" && consoletheme.IsBuiltin(setting.PublicHomeTheme) {
		return setting.PublicHomeTheme
	}
	return consoletheme.BuiltinDefault
}

func blogTitle(cfg BlogThemeConfig, fallback string) string {
	if cfg.BlogName != "" {
		return cfg.BlogName
	}
	return fallback
}

func (h *Handler) renderPublicHome(c *gin.Context, data publicHomeData) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Status(http.StatusOK)
	if err := h.tpl.ExecuteTemplate(c.Writer, "public_home.html", data); err != nil {
		c.String(http.StatusInternalServerError, "render failed")
	}
}

type publicArticle struct {
	ShareID     string
	models.File `gorm:"embedded"`
}

// publicArticleQuery 保持博客文章列表和网站地图使用相同的公开分享条件。
func publicArticleQuery(db *gorm.DB) *gorm.DB {
	return db.Table("shares").
		Joins("JOIN files ON files.user_id = shares.user_id AND files.vault_id = shares.vault_id AND files.path = shares.target_path").
		Where("shares.is_folder = ? AND shares.visibility IN ?", false, []string{"public", ""}).
		Where("files.is_deleted = ? AND files.type = ?", false, "markdown")
}

// homePosts 列出仓库中公开分享且目标仍存在的 Markdown 文章
func (h *Handler) homePosts(userID uint, vaultID string) []HomePost {
	var articles []publicArticle
	if err := publicArticleQuery(h.DB).Select("shares.share_id, files.*").
		Where("shares.user_id = ? AND shares.vault_id = ?", userID, vaultID).
		Order("shares.created_at desc").Order("shares.share_id asc").Scan(&articles).Error; err != nil {
		return nil
	}
	var posts []HomePost
	for _, article := range articles {
		f := article.File
		abs := filestore.DiskPath(h.Cfg.Storage.DataDir, f)
		raw, err := readFile(abs)
		if err != nil {
			continue
		}
		fm, body := splitFrontmatter(string(raw))
		assetResolver := blogAssetResolver{shareID: article.ShareID}
		title, meta := buildArticleMeta(fm, body, f.Path, f.UpdatedAt, assetResolver.ResolveAsset)
		posts = append(posts, HomePost{
			Title:     title,
			Summary:   meta.Summary,
			URL:       "/p/" + article.ShareID,
			Date:      meta.Date,
			Time:      f.UpdatedAt,
			Category:  meta.Category,
			Tags:      meta.Tags,
			CoverURL:  meta.CoverURL,
			WordCount: meta.WordCount,
		})
	}
	return posts
}

// extractPostMeta 提取文章标题与摘要；标题优先使用 frontmatter，否则使用文件名
func extractPostMeta(raw, fallbackTitle string) (string, string) {
	fm, body := splitFrontmatter(raw)
	title, meta := buildArticleMeta(fm, body, fallbackTitle, time.Time{}, nil)
	return title, meta.Summary
}

// handleVaultBlog 处理 /b/:vault_id 公开博客入口
func (h *Handler) handleVaultBlog(c *gin.Context) {
	vaultID := c.Param("vault_id")
	var vault models.Vault
	if err := h.DB.Where("id = ?", vaultID).First(&vault).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	var vs models.VaultSetting
	if err := h.DB.Where("vault_id = ?", vaultID).First(&vs).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if !vs.IsPublicBlog {
		c.Status(http.StatusNotFound)
		return
	}
	posts := h.homePosts(vault.OwnerID, vaultID)
	if h.pluginHooks != nil {
		if filtered, err := h.pluginHooks.ApplyHomePostsHook(c.Request.Context(), HomePostsHookPayload{
			VaultID: vaultID,
			Posts:   posts,
			Query:   c.Request.URL.Query(),
		}); err == nil {
			posts = filtered
		}
	}
	config := h.publicThemeConfig(vs.VaultID, vs.ThemeName, vs.ThemeConfig)
	cfg := ParseBlogThemeConfig(config)
	customEnabled := settingspolicy.CustomFragmentsEnabled(h.DB)
	homeThemeName := vs.ThemeName
	params := renderParams{
		VaultID:         vaultID,
		Title:           blogTitle(cfg, vault.Name),
		ThemeName:       homeThemeName,
		ThemeBaseURL:    themeBaseURL(homeThemeName),
		ThemeConfigJS:   template.JS(mustJSON(config)),
		CustomHeader:    renderSafeCustomFragmentEnabled(vs.CustomHeader, customEnabled),
		CustomFooter:    renderSafeCustomFragmentEnabled(vs.CustomFooter, customEnabled),
		IsHome:          true,
		BlogName:        cfg.BlogName,
		Description:     cfg.Description,
		LogoURL:         cfg.LogoURL,
		LogoSize:        cfg.LogoSize,
		LogoShape:       cfg.LogoShape,
		Buttons:         cfg.Buttons,
		HomePosts:       posts,
		BlogHomeURL:     "/b/" + vaultID,
		BannerURL:       cfg.BannerURL,
		MobileBannerURL: cfg.MobileBannerURL,
	}
	h.renderTemplate(c, params)
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
