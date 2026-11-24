package blog

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/config"
	"github.com/helantianshen/oss-sync/internal/filestore"
	"github.com/helantianshen/oss-sync/internal/models"
)

func TestSitemapFollowsPublicBlogVisibility(t *testing.T) {
	dir := t.TempDir()
	db, err := gorm.Open(sqlite.Open(filepath.Join(dir, "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.Vault{}, &models.VaultSetting{}, &models.File{}, &models.Share{}); err != nil {
		t.Fatal(err)
	}
	create := func(value any) {
		t.Helper()
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	modified := time.Date(2026, time.November, 24, 4, 0, 0, 0, time.UTC)
	create(&[]models.Vault{
		{ID: "listed", OwnerID: 1, Name: "Listed"},
		{ID: "unlisted", OwnerID: 1, Name: "Unlisted"},
		{ID: "archived", OwnerID: 1, Name: "Archived", ArchivedAt: gorm.DeletedAt{Time: modified, Valid: true}},
	})
	create(&[]models.VaultSetting{
		{VaultID: "listed", IsPublicBlog: true},
		{VaultID: "unlisted"},
		{VaultID: "archived", IsPublicBlog: true},
	})
	for _, item := range []struct {
		id, vault, visibility    string
		deleted, folder, missing bool
	}{
		{id: "article", vault: "listed", visibility: "public"},
		{id: "password", vault: "listed", visibility: "password"},
		{id: "login", vault: "listed", visibility: "login"},
		{id: "deleted", vault: "listed", visibility: "public", deleted: true},
		{id: "folder", vault: "listed", visibility: "public", folder: true},
		{id: "missing", vault: "listed", visibility: "public", missing: true},
		{id: "unlisted-article", vault: "unlisted", visibility: "public"},
		{id: "archived-article", vault: "archived", visibility: "public"},
	} {
		file := models.File{
			UserID: 1, VaultID: item.vault, Path: item.id + ".md", Type: "markdown",
			StorageKey: item.id + ".md", UpdatedAt: modified, IsDeleted: item.deleted,
		}
		create(&file)
		create(&models.Share{
			ShareID: item.id, UserID: 1, VaultID: item.vault, TargetPath: file.Path,
			Visibility: item.visibility, IsFolder: item.folder,
		})
		if !item.missing {
			if err := os.WriteFile(filestore.DiskPath(dir, file), []byte("# Article\n\nPublic body."), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	handler, err := New(db, &config.Config{Storage: config.StorageConfig{DataDir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	handler.Register(router)
	getWithProto := func(path, proto string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://notes.example.test"+path, nil)
		req.Header.Set("X-Forwarded-Proto", proto)
		req.Header.Set("X-Forwarded-Host", "untrusted.example.test")
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res
	}
	get := func(path string) *httptest.ResponseRecorder { return getWithProto(path, "https") }
	checkSitemap := func(want []sitemapURL) {
		t.Helper()
		res := get("/sitemap.xml")
		if res.Code != http.StatusOK || res.Header().Get("Content-Type") != "application/xml; charset=utf-8" {
			t.Fatalf("sitemap status %d, headers %v, body %s", res.Code, res.Header(), res.Body)
		}
		var got sitemapURLSet
		if err := xml.Unmarshal(res.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.XMLName.Space != sitemapNamespace || !reflect.DeepEqual(got.URLs, want) {
			t.Fatalf("unexpected sitemap: %s", res.Body)
		}
	}
	const site = "https://notes.example.test"
	want := []sitemapURL{
		{Location: site + "/"},
		{Location: site + "/b/listed"},
		{Location: site + "/p/article", LastMod: modified.Format(time.RFC3339)},
	}
	checkSitemap(want)
	if res := get("/robots.txt"); res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "Sitemap: "+site+"/sitemap.xml\n") {
		t.Fatalf("unexpected robots.txt: %d %s", res.Code, res.Body)
	}
	if posts := handler.homePosts(1, "listed"); len(posts) != 1 || posts[0].URL != "/p/article" {
		t.Fatalf("unexpected public blog posts: %+v", posts)
	}
	if err := db.Model(&models.Share{}).Where("share_id = ?", "article").Update("visibility", "login").Error; err != nil {
		t.Fatal(err)
	}
	checkSitemap(want[:2])
	if err := db.Model(&models.VaultSetting{}).Where("vault_id = ?", "listed").Update("is_public_blog", false).Error; err != nil {
		t.Fatal(err)
	}
	checkSitemap(want[:1])

	handler.Cfg.Server.PublicURL = "https://public.example.test"
	if res := getWithProto("/sitemap.xml", "http"); res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "<loc>https://public.example.test/</loc>") {
		t.Fatalf("unexpected sitemap behind HTTP proxy: %d %s", res.Code, res.Body)
	}
	if res := getWithProto("/robots.txt", "http"); res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "Sitemap: https://public.example.test/sitemap.xml\n") {
		t.Fatalf("unexpected robots.txt behind HTTP proxy: %d %s", res.Code, res.Body)
	}
}
