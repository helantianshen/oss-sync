package update

import (
	"errors"
	"testing"
)

func TestResolveDownloadURL(t *testing.T) {
	assetURL := "https://github.com/helantianshen/oss-sync/releases/download/1.2.3/asset.tar.gz"
	tests := []struct {
		name   string
		source string
		custom string
		want   string
		bad    bool
	}{
		{name: "default official", want: assetURL},
		{name: "official", source: "official", want: assetURL},
		{name: "built in proxy", source: "proxy", want: "https://gh-proxy.com/" + assetURL},
		{name: "custom", source: "custom", custom: "https://proxy.example.com/github/", want: "https://proxy.example.com/github/" + assetURL},
		{name: "custom requires https", source: "custom", custom: "http://proxy.example.com/", bad: true},
		{name: "custom rejects credentials", source: "custom", custom: "https://user:pass@proxy.example.com/", bad: true},
		{name: "unknown source", source: "other", bad: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveDownloadURL(assetURL, tc.source, tc.custom)
			if tc.bad {
				if !errors.Is(err, ErrInvalidURL) {
					t.Fatalf("expected invalid URL, got %v", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("resolveDownloadURL() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestResolveUpdateURL(t *testing.T) {
	rawURL := "https://api.github.com/repos/helantianshen/oss-sync/releases/latest"
	tests := []struct {
		name   string
		source string
		custom string
		want   string
		bad    bool
	}{
		{name: "official", source: "official", want: rawURL},
		{name: "built in proxy", source: "proxy", want: "https://gh-proxy.com/" + rawURL},
		{name: "custom prefix", source: "custom", custom: "https://mirror.example.com/github/", want: "https://mirror.example.com/github/" + rawURL},
		{name: "custom requires https", source: "custom", custom: "http://mirror.example.com/", bad: true},
		{name: "custom rejects query", source: "custom", custom: "https://mirror.example.com/?token=secret", bad: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveUpdateURL(rawURL, tc.source, tc.custom)
			if tc.bad {
				if !errors.Is(err, ErrInvalidURL) {
					t.Fatalf("expected invalid URL, got %v", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("resolveUpdateURL() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
