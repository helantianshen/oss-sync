package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, name, body string) {
	t.Helper()
	dir := t.TempDir()
	configsDir := filepath.Join(dir, "configs")
	if err := os.MkdirAll(configsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configsDir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
}

func TestLoad_Dev(t *testing.T) {
	writeConfig(t, "config.dev.yaml", `
server:
  host: "0.0.0.0"
  port: 8080
  mode: "debug"
  max_multipart_memory_mb: 8
  max_file_size_mb: 100
database:
  driver: "sqlite"
  dsn: "data/oss.db"
storage:
  data_dir: "data"
auth:
  jwt_secret: "secret"
  jwt_ttl_hours: 72
  allow_anonymous_registration: true
sync:
  max_concurrency: 6
`)
	t.Setenv("OSS_ENV", "dev")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Database.Driver != "sqlite" || cfg.Server.Port != 8080 {
		t.Errorf("unexpected cfg: %+v", cfg)
	}
	if !cfg.Auth.AllowAnonymousRegistration {
		t.Error("anonymous registration should be enabled from yaml")
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	writeConfig(t, "config.prod.yaml", `
server: {host: "0.0.0.0", port: 8080, mode: "release", max_multipart_memory_mb: 8, max_file_size_mb: 100}
database: {driver: "sqlite", dsn: "data/oss.db"}
storage: {data_dir: "data"}
auth: {jwt_secret: "from-file", jwt_ttl_hours: 72, allow_anonymous_registration: true}
sync: {max_concurrency: 6}
`)
	t.Setenv("OSS_ENV", "prod")
	t.Setenv("OSS_JWT_SECRET", "from-env")
	t.Setenv("OSS_ALLOW_ANONYMOUS_REGISTRATION", "false")
	t.Setenv("OSS_SERVER_PORT", "9999")
	t.Setenv("OSS_STORAGE_DIR", "/var/data")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Auth.JWTSecret != "from-env" {
		t.Errorf("jwt secret not overridden: %q", cfg.Auth.JWTSecret)
	}
	if cfg.Auth.AllowAnonymousRegistration {
		t.Error("anonymous registration should be disabled by env override")
	}
	if cfg.Server.Port != 9999 {
		t.Errorf("port not overridden: %d", cfg.Server.Port)
	}
	if cfg.Storage.DataDir != "/var/data" {
		t.Errorf("data dir not overridden: %q", cfg.Storage.DataDir)
	}
}

func TestLoad_InvalidAnonymousRegistrationEnv(t *testing.T) {
	writeConfig(t, "config.dev.yaml", `
server: {host: "0.0.0.0", port: 8080, mode: "debug"}
database: {driver: "sqlite", dsn: "data/oss.db"}
storage: {data_dir: "data"}
auth: {jwt_secret: "secret", allow_anonymous_registration: false}
`)
	t.Setenv("OSS_ENV", "dev")
	t.Setenv("OSS_ALLOW_ANONYMOUS_REGISTRATION", "enabled")
	if _, err := Load(); err == nil {
		t.Error("expected error for invalid anonymous registration environment value")
	}
}

func TestLoad_AnonymousRegistrationEnvEnables(t *testing.T) {
	writeConfig(t, "config.prod.yaml", `
server: {host: "0.0.0.0", port: 8080, mode: "release"}
database: {driver: "sqlite", dsn: "data/oss.db"}
storage: {data_dir: "data"}
auth: {jwt_secret: "secret", allow_anonymous_registration: false}
`)
	t.Setenv("OSS_ENV", "prod")
	t.Setenv("OSS_ALLOW_ANONYMOUS_REGISTRATION", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Auth.AllowAnonymousRegistration {
		t.Error("anonymous registration should be enabled by env override")
	}
}

func TestLoad_InvalidEnv(t *testing.T) {
	t.Setenv("OSS_ENV", "staging")
	if _, err := Load(); err == nil {
		t.Error("expected error for invalid env")
	}
}

func TestLoad_MissingConfigFile(t *testing.T) {
	t.Setenv("OSS_ENV", "dev")
	t.Chdir(t.TempDir())
	if _, err := Load(); err == nil {
		t.Error("expected error for missing config file")
	}
}

func TestLoad_ValidationFails(t *testing.T) {
	writeConfig(t, "config.dev.yaml", `
server: {host: "", port: 8080, mode: "debug"}
database: {driver: "mysql", dsn: ""}
storage: {data_dir: ""}
auth: {jwt_secret: ""}
`)
	t.Setenv("OSS_ENV", "dev")
	if _, err := Load(); err == nil {
		t.Error("expected validation error")
	}
}

func TestEnv_Default(t *testing.T) {
	t.Setenv("OSS_ENV", "")
	if Env() != "dev" {
		t.Errorf("default env should be dev, got %s", Env())
	}
}
