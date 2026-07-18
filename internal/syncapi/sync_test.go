package syncapi

import "testing"

func TestClassifyFile(t *testing.T) {
	cases := map[string]string{
		"Note.md":                "markdown",
		"Notes/Front/Go.md":      "markdown",
		".obsidian/app.json":     "config",
		".obsidian/themes/x.css": "config",
		"img/photo.png":          "attachment",
		"doc.pdf":                "attachment",
		"video/clip.mp4":         "attachment",
	}
	for in, want := range cases {
		if got := classifyFile(in); got != want {
			t.Errorf("classifyFile(%q)=%q want %q", in, got, want)
		}
	}
}

func TestIsSafeRelativePath(t *testing.T) {
	good := []string{
		"Note.md",
		"Notes/Front/Go.md",
		".obsidian/app.json",
		"a/b/c/d.png",
	}
	bad := []string{
		"",
		"/etc/passwd",
		"../escape.md",
		"../",
		"a/../../b.md",
		"C:\\Windows\\x",
		"a:b",
	}
	for _, p := range good {
		if !isSafeRelativePath(p) {
			t.Errorf("expected safe: %q", p)
		}
	}
	for _, p := range bad {
		if isSafeRelativePath(p) {
			t.Errorf("expected unsafe: %q", p)
		}
	}
}
