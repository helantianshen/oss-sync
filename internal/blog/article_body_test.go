package blog

import (
	"strings"
	"testing"
)

func TestRenderArticleBodyUsesSandboxOnlyForRawHTML(t *testing.T) {
	const article = "## 标题\n\n==路线==\n\n![地图](map.png)\n\n```html\n<script>example()</script>\n```"
	for _, allowRaw := range []bool{false, true} {
		rendered, err := renderArticleBody(nil, nil, article, allowRaw, "/themes/default")
		if err != nil {
			t.Fatal(err)
		}
		html := string(rendered)
		if strings.Contains(html, "oss-article-sandbox") || !strings.Contains(html, "<img") || !strings.Contains(html, "<mark>路线</mark>") {
			t.Fatalf("unexpected Markdown output: %s", html)
		}
	}

	const raw = "<div>正文</div>\n<script>example()</script>"
	rendered, err := renderArticleBody(nil, nil, raw, true, "/themes/default")
	if err != nil {
		t.Fatal(err)
	}
	html := string(rendered)
	if !strings.Contains(html, `sandbox="allow-scripts"`) ||
		!strings.Contains(html, "data-oss-frame-child") ||
		!strings.Contains(html, "&lt;script&gt;example()&lt;/script&gt;") {
		t.Fatalf("raw HTML must render inside the sandbox: %s", html)
	}
	rendered, err = renderArticleBody(nil, nil, raw, false, "/themes/default")
	if err != nil || strings.Contains(string(rendered), "<script>") || strings.Contains(string(rendered), "oss-article-sandbox") {
		t.Fatalf("raw HTML was not suppressed: %s, %v", rendered, err)
	}
}
