package blog

import (
	_ "embed"
	"html/template"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/helantianshen/oss-sync/internal/markdown"
)

const articleFrameScriptURL = "/blog-assets/article-frame.js"
const articleFrameScript = `<script defer src="` + articleFrameScriptURL + `?v=1"`

//go:embed assets/article-frame.js
var articleFrameJS []byte

func serveArticleFrameScript(c *gin.Context) {
	c.Data(http.StatusOK, "text/javascript; charset=utf-8", articleFrameJS)
}

func renderArticleBody(resolver markdown.LinkResolver, assets markdown.AssetResolver, source string, allowRawHTML bool, themeBaseURL string) (template.HTML, error) {
	safe, err := markdown.RenderMarkdownWithAssets(resolver, assets, source)
	if err != nil || !allowRawHTML {
		return template.HTML(safe), err
	}
	raw, err := markdown.RenderMarkdownWithAssetsUnsafe(resolver, assets, source)
	if err != nil {
		return "", err
	}
	// 只有启用原始 HTML 确实改变输出时，才需要沙箱。
	if raw == safe {
		return template.HTML(safe), nil
	}
	return template.HTML(sandboxArticleHTML(raw, themeBaseURL)), nil
}

func sandboxArticleHTML(content, themeBaseURL string) string {
	styleHref := ""
	if themeBaseURL != "" {
		styleHref = `<link rel="stylesheet" href="` + htmlEscape(themeBaseURL+"/style.css") + `">`
	}
	doc := `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">` +
		styleHref + `<style>
html { color-scheme: light dark; }
body.oss-content { margin: 0; background: transparent; display: flow-root; overflow-wrap: anywhere; }
.oss-content :where(img, video, svg, iframe, audio) { max-width: 100%; }
.oss-content :where(img, video) { height: auto; }
.oss-content :where(pre, table) { max-width: 100%; overflow-x: auto; }
.oss-content :where(table) { display: block; }
</style>` + articleFrameScript + ` data-oss-frame-child></script></head><body class="oss-content">` + content + `</body></html>`
	// 不授予 allow-same-origin，避免正文脚本读取父页会话及 DOM。
	return articleFrameScript + `></script><iframe class="oss-article-sandbox" title="文章内容" loading="lazy"` +
		` style="display:block;width:100%;height:min(70vh,45rem);border:0" sandbox="allow-scripts"` +
		` srcdoc="` + htmlEscapeAttribute(doc) + `"></iframe>`
}
