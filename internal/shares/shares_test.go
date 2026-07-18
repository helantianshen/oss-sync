package shares

import "testing"

func TestIsSafeSharePath(t *testing.T) {
	good := []string{
		"Note.md",
		"Notes/Front/Go.md",
		"Notes/",
		"a/b/c/d.png",
	}
	bad := []string{
		"",
		"/etc/passwd",
		"../escape.md",
		"../",
		"a/../../b.md",
		"C:\\Windows\\x",
	}
	for _, p := range good {
		if !isSafeSharePath(p) {
			t.Errorf("expected safe: %q", p)
		}
	}
	for _, p := range bad {
		if isSafeSharePath(p) {
			t.Errorf("expected unsafe: %q", p)
		}
	}
}

func TestNormalizeRelPath(t *testing.T) {
	cases := map[string]string{
		"./a/b.png":  "a/b.png",
		"\\a\\b.png": "a/b.png",
		"/a/b.png":   "a/b.png",
		"a/../b.png": "b.png",
	}
	for in, want := range cases {
		if got := normalizeRelPath(in); got != want {
			t.Errorf("normalizeRelPath(%q)=%q want %q", in, got, want)
		}
	}
}

func TestGenShareID_Format(t *testing.T) {
	ids := map[string]struct{}{}
	for i := 0; i < 100; i++ {
		id, err := genShareID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != shareIDLen {
			t.Errorf("len=%d want %d", len(id), shareIDLen)
		}
		for _, c := range id {
			isAlnum := (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
			if !isAlnum {
				t.Errorf("non-base62 char in %q", id)
			}
		}
		ids[id] = struct{}{}
	}
	if len(ids) < 50 {
		t.Errorf("too many collisions: only %d unique out of 100", len(ids))
	}
}
