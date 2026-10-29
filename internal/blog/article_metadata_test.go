package blog

import (
	"html/template"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/html"

	"github.com/helantianshen/oss-sync/internal/config"
)

func TestArticleMetadataManualAndAutomatic(t *testing.T) {
	manual := "---\ntitle: 手写标题\ndescription: 手写摘要\n---\n# 正文标题\n\n自动正文。"
	title, summary := extractPostMeta(manual, "教程.md")
	if title != "手写标题" || summary != "手写摘要" {
		t.Fatalf("manual metadata lost: %q, %q", title, summary)
	}
	automatic := "---\ntitle: '  '\ndescription: '  '\n---\n### 目录\n\n[[#基础使用教程]]\n\n# 基础使用教程\n\n- 点击==`绘制路线`==按钮，进入绘制界面。"
	title, summary = extractPostMeta(automatic, "sport使用手册.md")
	if title != "sport使用手册" || summary != "点击绘制路线按钮，进入绘制界面。" {
		t.Fatalf("unexpected automatic metadata: %q, %q", title, summary)
	}
	fm, body := splitFrontmatter("---\nsummary: 自定义摘要别名\n---\n正文。")
	_, meta := buildArticleMeta(fm, body, "文章.md", time.Time{}, nil)
	if meta.Summary != "自定义摘要别名" {
		t.Fatalf("summary alias lost: %q", meta.Summary)
	}
}

func TestArticleDescriptionAcrossBuiltinThemes(t *testing.T) {
	h, err := New(nil, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, title, summary, want string
		home, folder               bool
	}{
		{name: "article", title: "使用手册", summary: "从正文提取的文章摘要。", want: "从正文提取的文章摘要。"},
		{name: "empty article", title: "相册", want: "相册"},
		{name: "home", home: true, title: "首页", summary: "文章摘要", want: "统一博客介绍"},
		{name: "folder", folder: true, title: "目录", want: "统一博客介绍"},
		{name: "escaped", title: `标题"><script data-seo-injection>alert(1)</script>`, summary: `摘要"><script data-seo-injection>alert(2)</script>`, want: `摘要"><script data-seo-injection>alert(2)</script>`},
	}
	for _, theme := range BuiltinThemeNames {
		for _, tc := range cases {
			t.Run(theme+"/"+tc.name, func(t *testing.T) {
				response := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(response)
				c.Request = httptest.NewRequest("GET", "/p/test", nil)
				h.renderTemplate(c, renderParams{
					Title: tc.title + " · OSS", ArticleTitle: tc.title,
					ThemeName: theme, ThemeBaseURL: "/themes/" + theme,
					Description: "统一博客介绍", ArticlePost: ArticleMeta{Summary: tc.summary},
					IsHome: tc.home, IsFolder: tc.folder, BlogName: "示例博客",
					ContentHTML: template.HTML("<p>正文</p>"),
				})
				if response.Code != 200 || !strings.Contains(response.Body.String(), "/themes/"+theme+"/style.css") {
					t.Fatalf("theme %q did not render successfully: HTTP %d", theme, response.Code)
				}
				if got := response.Header().Get("X-Robots-Tag"); got != "index, follow" {
					t.Fatalf("unexpected indexing directive: %q", got)
				}
				doc, err := html.Parse(strings.NewReader(response.Body.String()))
				if err != nil {
					t.Fatal(err)
				}
				var descriptions []string
				var visit func(*html.Node)
				visit = func(n *html.Node) {
					attrs := make(map[string]string)
					for _, attr := range n.Attr {
						attrs[attr.Key] = attr.Val
					}
					if _, injected := attrs["data-seo-injection"]; injected {
						t.Error("metadata created an HTML element")
					}
					if n.Type == html.ElementNode && n.Data == "meta" && attrs["name"] == "description" {
						descriptions = append(descriptions, attrs["content"])
					}
					for child := n.FirstChild; child != nil; child = child.NextSibling {
						visit(child)
					}
				}
				visit(doc)
				if len(descriptions) != 1 || descriptions[0] != tc.want {
					t.Fatalf("descriptions=%q, want exactly %q", descriptions, tc.want)
				}
			})
		}
	}
}
