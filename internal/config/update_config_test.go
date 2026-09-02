package config

import (
	"testing"
	"time"
)

func TestUpdateConfig_EffectiveDefaults(t *testing.T) {
	var c UpdateConfig
	if got := c.EffectiveGitHubRepo(); got != "helantianshen/oss-sync" {
		t.Errorf("EffectiveGitHubRepo default = %q", got)
	}
	if got := c.EffectiveTimeout(); got != 15*time.Second {
		t.Errorf("EffectiveTimeout default = %v", got)
	}
	if got := c.EffectiveUpdateTimeout(); got != 600*time.Second {
		t.Errorf("EffectiveUpdateTimeout default = %v", got)
	}
	if got := c.EffectiveCheckTTL(); got != 3600*time.Second {
		t.Errorf("EffectiveCheckTTL default = %v", got)
	}
	if got := c.EffectiveCheckLimit(); got != 6 {
		t.Errorf("EffectiveCheckLimit default = %d", got)
	}
	if got := c.EffectiveCheckWindow(); got != time.Minute {
		t.Errorf("EffectiveCheckWindow default = %v", got)
	}
	if got := c.EffectiveDownloadSource(); got != "official" {
		t.Errorf("EffectiveDownloadSource default = %q", got)
	}
}

func TestUpdateConfig_EffectiveDownloadSource(t *testing.T) {
	c := UpdateConfig{DownloadSource: " proxy ", DownloadProxy: " https://mirror.example.com/ "}
	if got := c.EffectiveDownloadSource(); got != "proxy" {
		t.Errorf("EffectiveDownloadSource = %q, want proxy", got)
	}
	if got := c.EffectiveDownloadProxy(); got != "https://mirror.example.com/" {
		t.Errorf("EffectiveDownloadProxy = %q", got)
	}
}

func TestUpdateConfig_EffectiveBounded(t *testing.T) {
	c := UpdateConfig{
		TimeoutSeconds:       30,
		UpdateTimeoutSeconds: 120,
		CheckTTLSeconds:      600,
		CheckLimit:           10,
		CheckWindowSeconds:   120,
	}
	if got := c.EffectiveTimeout(); got != 30*time.Second {
		t.Errorf("EffectiveTimeout = %v", got)
	}
	if got := c.EffectiveUpdateTimeout(); got != 120*time.Second {
		t.Errorf("EffectiveUpdateTimeout = %v", got)
	}
	if got := c.EffectiveCheckTTL(); got != 600*time.Second {
		t.Errorf("EffectiveCheckTTL = %v", got)
	}
	// Out of bounds should fallback to default
	c = UpdateConfig{TimeoutSeconds: 1, UpdateTimeoutSeconds: 10, CheckTTLSeconds: 10, CheckLimit: 200, CheckWindowSeconds: 5}
	if got := c.EffectiveTimeout(); got != 15*time.Second {
		t.Errorf("out of bounds EffectiveTimeout should default, got %v", got)
	}
	if got := c.EffectiveUpdateTimeout(); got != 600*time.Second {
		t.Errorf("out of bounds EffectiveUpdateTimeout default, got %v", got)
	}
	if got := c.EffectiveCheckTTL(); got != 3600*time.Second {
		t.Errorf("out of bounds EffectiveCheckTTL default, got %v", got)
	}
	if got := c.EffectiveCheckLimit(); got != 6 {
		t.Errorf("out of bounds EffectiveCheckLimit default, got %d", got)
	}
	if got := c.EffectiveCheckWindow(); got != time.Minute {
		t.Errorf("out of bounds EffectiveCheckWindow default, got %v", got)
	}
}

func TestUpdateConfig_Validate_Repo(t *testing.T) {
	valid := []string{"helantianshen/oss-sync", "my-org/my.repo", "a/b", "owner_1/repo-2"}
	for _, r := range valid {
		c := UpdateConfig{GitHubRepo: r}
		if err := c.validate(); err != nil {
			t.Errorf("valid repo %q should pass: %v", r, err)
		}
	}
	invalid := []string{"", "owner", "owner/", "/repo", "owner/repo/extra", "owner//repo", "-owner/repo", "owner/-repo", "owner/repo with space"}
	// Empty is allowed (uses default), so skip empty for invalid check beyond owner/
	for _, r := range invalid[1:] {
		c := UpdateConfig{GitHubRepo: r}
		if err := c.validate(); err == nil {
			t.Errorf("invalid repo %q should fail", r)
		}
	}
	// Check bound validation
	c := UpdateConfig{TimeoutSeconds: 200}
	if err := c.validate(); err == nil {
		t.Error("timeout out of bounds should fail")
	}
	c = UpdateConfig{UpdateTimeoutSeconds: 5}
	if err := c.validate(); err == nil {
		t.Error("update timeout out of bounds should fail")
	}
	c = UpdateConfig{CheckTTLSeconds: 10}
	if err := c.validate(); err == nil {
		t.Error("check ttl out of bounds should fail")
	}
}

func TestUpdateConfig_Validate_DownloadSource(t *testing.T) {
	valid := []UpdateConfig{
		{DownloadSource: "official"},
		{DownloadSource: "proxy"},
		{DownloadSource: "custom", DownloadProxy: "https://mirror.example.com/github/"},
	}
	for _, c := range valid {
		if err := c.validate(); err != nil {
			t.Errorf("valid download source should pass: %+v: %v", c, err)
		}
	}
	invalid := []UpdateConfig{
		{DownloadSource: "mirror"},
		{DownloadSource: "custom"},
		{DownloadSource: "custom", DownloadProxy: "http://mirror.example.com/"},
		{DownloadSource: "custom", DownloadProxy: "https://user:pass@mirror.example.com/"},
		{DownloadSource: "custom", DownloadProxy: "https://mirror.example.com/?token=secret"},
	}
	for _, c := range invalid {
		if err := c.validate(); err == nil {
			t.Errorf("invalid download source should fail: %+v", c)
		}
	}
}

func TestConfig_Validate_UpdateIntegration(t *testing.T) {
	cfg := &Config{
		Server:   ServerConfig{Port: 8080},
		Database: DatabaseConfig{Driver: "sqlite", DSN: "data/oss.db"},
		Storage:  StorageConfig{DataDir: "data"},
		Update:   UpdateConfig{GitHubRepo: "bad repo"},
	}
	if err := cfg.validate(); err == nil {
		t.Error("cfg.validate should fail for bad repo")
	}
	cfg.Update = UpdateConfig{GitHubRepo: "helantianshen/oss-sync", TimeoutSeconds: 15}
	if err := cfg.validate(); err != nil {
		t.Errorf("valid cfg should pass: %v", err)
	}
}
