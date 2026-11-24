package blog

import (
	"encoding/xml"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/filestore"
)

const sitemapNamespace = "http://www.sitemaps.org/schemas/sitemap/0.9"
const sitemapPageSize = 50000

type sitemapURL struct {
	Location string `xml:"loc"`
	LastMod  string `xml:"lastmod,omitempty"`
}

type sitemapURLSet struct {
	XMLName xml.Name     `xml:"urlset"`
	XMLNS   string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

type sitemapIndex struct {
	XMLName  xml.Name     `xml:"sitemapindex"`
	XMLNS    string       `xml:"xmlns,attr"`
	Sitemaps []sitemapURL `xml:"sitemap"`
}

// sitemapOrigin 优先使用配置的公网地址，未配置时使用请求域名和代理协议。
func (h *Handler) sitemapOrigin(r *http.Request) *url.URL {
	if h.Cfg.Server.PublicURL != "" {
		origin, _ := url.Parse(h.Cfg.Server.PublicURL)
		return origin
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		scheme = "https"
	}
	origin, err := url.Parse(scheme + "://" + r.Host)
	if err != nil || origin.Host == "" || origin.Host != r.Host {
		return nil
	}
	return origin
}

func sitemapLocation(origin *url.URL, path string, page int) string {
	location := *origin
	location.Path = path
	if page > 0 {
		location.RawQuery = "page=" + strconv.Itoa(page)
	}
	return location.String()
}

func (h *Handler) handleRobots(c *gin.Context) {
	origin := h.sitemapOrigin(c.Request)
	if origin == nil {
		c.Status(http.StatusBadRequest)
		return
	}
	c.String(http.StatusOK, "User-agent: *\nDisallow:\n\nSitemap: %s\n", sitemapLocation(origin, "/sitemap.xml", 0))
}

func (h *Handler) handleSitemap(c *gin.Context) {
	origin := h.sitemapOrigin(c.Request)
	if origin == nil {
		c.Status(http.StatusBadRequest)
		return
	}
	page := 0
	if raw, exists := c.GetQuery("page"); exists {
		var err error
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 {
			c.Status(http.StatusNotFound)
			return
		}
	}

	db := h.DB.WithContext(c.Request.Context())
	const publicVaultJoin = "JOIN vault_settings ON vault_settings.vault_id = vaults.id AND vault_settings.is_public_blog = ? AND vaults.archived_at IS NULL"
	vaults := db.Table("vaults").Joins(publicVaultJoin, true).Session(&gorm.Session{})
	articles := publicArticleQuery(db).
		Joins("JOIN vaults ON vaults.id = shares.vault_id AND vaults.owner_id = shares.user_id").
		Joins(publicVaultJoin, true).Session(&gorm.Session{})
	var vaultCount, articleCount int64
	if vaults.Count(&vaultCount).Error != nil || articles.Count(&articleCount).Error != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	pages := (vaultCount+articleCount)/sitemapPageSize + 1
	if page == 0 && pages > 1 {
		index := sitemapIndex{XMLNS: sitemapNamespace}
		for p := 1; int64(p) <= pages; p++ {
			index.Sitemaps = append(index.Sitemaps, sitemapURL{Location: sitemapLocation(origin, "/sitemap.xml", p)})
		}
		writeSitemapXML(c, index)
		return
	}
	if int64(page) > pages || (page > 0 && pages == 1) {
		c.Status(http.StatusNotFound)
		return
	}

	offset := max(page-1, 0) * sitemapPageSize
	entries, err := h.sitemapPage(origin, vaults, articles, int(vaultCount), offset)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	writeSitemapXML(c, sitemapURLSet{XMLNS: sitemapNamespace, URLs: entries})
}

func (h *Handler) sitemapPage(origin *url.URL, vaults, articles *gorm.DB, vaultCount, offset int) ([]sitemapURL, error) {
	var entries []sitemapURL
	if offset == 0 {
		entries = append(entries, sitemapURL{Location: sitemapLocation(origin, "/", 0)})
	}
	if vaultOffset := max(offset-1, 0); vaultOffset < vaultCount {
		var ids []string
		if err := vaults.Order("vaults.id").Offset(vaultOffset).Limit(sitemapPageSize-len(entries)).Pluck("vaults.id", &ids).Error; err != nil {
			return nil, err
		}
		for _, id := range ids {
			entries = append(entries, sitemapURL{Location: sitemapLocation(origin, "/b/"+id, 0)})
		}
	}
	var files []publicArticle
	if remaining := sitemapPageSize - len(entries); remaining > 0 {
		if err := articles.Select("shares.share_id, files.user_id, files.path, files.storage_key, files.updated_at").
			Order("shares.share_id").Offset(max(offset-1-vaultCount, 0)).Limit(remaining).Scan(&files).Error; err != nil {
			return nil, err
		}
	}
	for _, file := range files {
		info, err := os.Stat(filestore.DiskPath(h.Cfg.Storage.DataDir, file.File))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		entry := sitemapURL{Location: sitemapLocation(origin, "/p/"+file.ShareID, 0)}
		if !file.UpdatedAt.IsZero() {
			entry.LastMod = file.UpdatedAt.UTC().Format(time.RFC3339)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func writeSitemapXML(c *gin.Context, data any) {
	content, err := xml.Marshal(data)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/xml; charset=utf-8", append([]byte(xml.Header), content...))
}
