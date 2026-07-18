package cron

import (
	"sort"
	"testing"
)

func TestExtractAttachmentRefs_AllFourTypes(t *testing.T) {
	md := `---
title: Note
cover: cover.png
image: images/shot.png
banner: ./banners/promo.jpg
---
# Hello

![alt](plain.png)
![[wikilink.png]]
![[wikilink2.png|200]]
<img src="html.png" alt="x">
<img class="c" src='single-quote.jpg'>

text ![](https://example.com/remote.png) text
`
	refs := extractAttachmentRefs(md, "Notes/Front/Go.md")
	got := uniqueSorted(refs)

	want := uniqueSorted([]string{
		"Notes/Front/cover.png",
		"Notes/Front/images/shot.png",
		"Notes/Front/banners/promo.jpg",
		"Notes/Front/plain.png",
		"Notes/Front/wikilink.png",
		"Notes/Front/wikilink2.png",
		"Notes/Front/html.png",
		"Notes/Front/single-quote.jpg",
	})
	if len(got) != len(want) {
		t.Fatalf("count mismatch: got %d %v, want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("idx %d: got %q want %q", i, got[i], w)
		}
	}
}

func TestExtractAttachmentRefs_NoFrontmatter(t *testing.T) {
	md := `# No YAML

![](a.png)
`
	refs := extractAttachmentRefs(md, "Note.md")
	want := []string{"a.png"}
	if len(refs) != 1 || refs[0] != want[0] {
		t.Errorf("got %v, want %v", refs, want)
	}
}

func TestExtractAttachmentRefs_DotDotResolution(t *testing.T) {
	md := `![](../sibling.png)`
	refs := extractAttachmentRefs(md, "dir/sub/Note.md")
	got := uniqueSorted(refs)
	want := []string{"dir/sibling.png"}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractAttachmentRefs_RemoteUrlsIgnored(t *testing.T) {
	md := `![](https://x.com/a.png)
![](http://y.com/b.png)
`
	refs := extractAttachmentRefs(md, "Note.md")
	if len(refs) != 0 {
		t.Errorf("remote urls should be ignored, got %v", refs)
	}
}

func TestSplitFrontmatter(t *testing.T) {
	fm, body := splitFrontmatter("---\ncover: c.png\n---\nbody")
	if fm != "cover: c.png" || body != "body" {
		t.Errorf("fm=%q body=%q", fm, body)
	}
	fm2, body2 := splitFrontmatter("no fm")
	if fm2 != "" || body2 != "no fm" {
		t.Errorf("fm=%q body=%q", fm2, body2)
	}
}

func TestNormalizeRel(t *testing.T) {
	cases := map[string]string{
		"./a/b.png":   "a/b.png",
		"\\a\\b.png":  "a/b.png",
		"/a/b.png":    "a/b.png",
		"a/../b.png":  "b.png",
	}
	for in, want := range cases {
		if got := normalizeRel(in); got != want {
			t.Errorf("normalizeRel(%q)=%q want %q", in, got, want)
		}
	}
}

func uniqueSorted(in []string) []string {
	set := map[string]struct{}{}
	for _, x := range in {
		set[x] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
