package markdown

import (
	"net/url"
	"strings"
	"testing"
)

type fakeResolver struct {
	idx map[string]string
}

func (r *fakeResolver) Resolve(linkText string) string {
	return r.idx[linkText]
}

func TestWikilinkRenders(t *testing.T) {
	r := &fakeResolver{idx: map[string]string{"world": "bbb222"}}
	html, err := RenderMarkdown(r, `引用了 [[world]] 与 [[not-exist]]`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := `<a href="/p/bbb222" target="_blank">world</a>`
	if !strings.Contains(html, want) {
		t.Errorf("expected link, got:\n%s", html)
	}
	want2 := `<span class="unshared-link">not-exist(未分享)</span>`
	if !strings.Contains(html, want2) {
		t.Errorf("expected unshared, got:\n%s", html)
	}
}

func TestWikilinkAlias(t *testing.T) {
	r := &fakeResolver{idx: map[string]string{"world": "bbb222"}}
	html, err := RenderMarkdown(r, `[[world|世界]]`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(html, `href="/p/bbb222"`) {
		t.Errorf("alias: link should point to bbb222, got:\n%s", html)
	}
	if !strings.Contains(html, `>世界</a>`) {
		t.Errorf("alias: display text should be 世界, got:\n%s", html)
	}
}

func TestWikilinkNoResolver(t *testing.T) {
	html, err := RenderMarkdown(nil, `[[anything]]`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(html, `<span class="unshared-link">anything(未分享)</span>`) {
		t.Errorf("nil resolver: got:\n%s", html)
	}
}

func TestWikilinkAdjacent(t *testing.T) {
	r := &fakeResolver{idx: map[string]string{"a": "id1", "b": "id2"}}
	html, err := RenderMarkdown(r, `[[a]][[b]]`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(html, `<a href="/p/id1" target="_blank">a</a><a href="/p/id2" target="_blank">b</a>`) {
		t.Errorf("adjacent: got:\n%s", html)
	}
}

type fakeAssetResolver struct{}

func (fakeAssetResolver) ResolveAsset(reference string) string {
	escaped := strings.ReplaceAll(url.QueryEscape(reference), "+", "%20")
	escaped = strings.ReplaceAll(escaped, "%2F", "/")
	return "/assets/share?ref=" + escaped
}

func TestObsidianImageEmbedRendersImage(t *testing.T) {
	html, err := RenderMarkdownWithAssets(nil, fakeAssetResolver{}, `![[Pasted image.png|640]]`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, `<img src="/assets/share?ref=Pasted%20image.png" alt="Pasted image.png">`) {
		t.Errorf("expected image embed, got %s", html)
	}
}

func TestStandardMarkdownImageUsesAssetResolver(t *testing.T) {
	html, err := RenderMarkdownWithAssets(nil, fakeAssetResolver{}, `![diagram](static/a.png)`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, `<img src="/assets/share?ref=static/a.png" alt="diagram">`) {
		t.Errorf("expected resolved image, got %s", html)
	}
}

func TestReferencedAssetsIgnoresCodeBlocks(t *testing.T) {
	references, err := ReferencedAssets("```markdown\n![[secret.png]]\n```\n\n![[public.png]]")
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 1 || references[0] != "public.png" {
		t.Fatalf("expected only public image reference, got %v", references)
	}
}
