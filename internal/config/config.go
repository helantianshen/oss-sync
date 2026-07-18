package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 是后端运行配置，按 OSS_ENV 加载开发或生产配置文件。
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Storage  StorageConfig  `yaml:"storage"`
	Auth     AuthConfig     `yaml:"auth"`
	Sync     SyncConfig     `yaml:"sync"`
}

type ServerConfig struct {
	Host                 string `yaml:"host"`
	Port                 int    `yaml:"port"`
	Mode                 string `yaml:"mode"`
	MaxMultipartMemoryMB int64  `yaml:"max_multipart_memory_mb"`
	MaxFileSizeMB        int64  `yaml:"max_file_size_mb"`
}

type DatabaseConfig struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

type StorageConfig struct {
	DataDir string `yaml:"data_dir"`
}

type AuthConfig struct {
	JWTSecret                  string `yaml:"jwt_secret"`
	JWTTTLHours                int    `yaml:"jwt_ttl_hours"`
	AllowAnonymousRegistration bool   `yaml:"allow_anonymous_registration"`
}

type SyncConfig struct {
	MaxConcurrency         int `yaml:"max_concurrency"`
	DeviceStaleDays        int `yaml:"device_stale_days"`
	ReconcileIntervalHours int `yaml:"reconcile_interval_hours"`
	TempFileMaxAgeHours    int `yaml:"temp_file_max_age_hours"`
	OrphanFileGraceHours   int `yaml:"orphan_file_grace_hours"`
}

// Load 读取与 OSS_ENV 对应的配置文件并合并环境变量覆盖。
//
// OSS_ENV 取值：dev（默认）/ prod。对应 configs/config.<env>.yaml。
// 配置文件查找路径：configs/config.<env>.yaml（相对于工作目录）。
// 以下字段支持环境变量覆盖：
//   - OSS_JWT_SECRET
//   - OSS_ALLOW_ANONYMOUS_REGISTRATION
//   - OSS_DB_DRIVER / OSS_DB_DSN
//   - OSS_SERVER_HOST / OSS_SERVER_PORT
//   - OSS_STORAGE_DIR
func Load() (*Config, error) {
	env := os.Getenv("OSS_ENV")
	if env == "" {
		env = "dev"
	}
	if env != "dev" && env != "prod" {
		return nil, fmt.Errorf("OSS_ENV 仅支持 dev / prod，收到 %q", env)
	}

	path := filepath.Join("configs", fmt.Sprintf("config.%s.yaml", env))
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}

	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}

	if err := c.applyEnvOverrides(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyEnvOverrides() error {
	if v := os.Getenv("OSS_JWT_SECRET"); v != "" {
		c.Auth.JWTSecret = v
	}
	if v, ok := os.LookupEnv("OSS_ALLOW_ANONYMOUS_REGISTRATION"); ok && v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true":
			c.Auth.AllowAnonymousRegistration = true
		case "false":
			c.Auth.AllowAnonymousRegistration = false
		default:
			return fmt.Errorf(
				"OSS_ALLOW_ANONYMOUS_REGISTRATION 必须为 true 或 false，收到 %q",
				v,
			)
		}
	}
	if v := os.Getenv("OSS_DB_DRIVER"); v != "" {
		c.Database.Driver = strings.ToLower(v)
	}
	if v := os.Getenv("OSS_DB_DSN"); v != "" {
		c.Database.DSN = v
	}
	if v := os.Getenv("OSS_SERVER_HOST"); v != "" {
		c.Server.Host = v
	}
	if v := os.Getenv("OSS_SERVER_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			c.Server.Port = port
		}
	}
	if v := os.Getenv("OSS_STORAGE_DIR"); v != "" {
		c.Storage.DataDir = v
	}
	if v := os.Getenv("OSS_DEVICE_STALE_DAYS"); v != "" {
		if days, err := strconv.Atoi(v); err == nil {
			c.Sync.DeviceStaleDays = days
		}
	}
	if v := os.Getenv("OSS_RECONCILE_INTERVAL_HOURS"); v != "" {
		if hours, err := strconv.Atoi(v); err == nil {
			c.Sync.ReconcileIntervalHours = hours
		}
	}
	return nil
}

func (c *Config) validate() error {
	if c.Database.Driver == "" {
		return fmt.Errorf("database.driver 不能为空")
	}
	if c.Database.Driver != "sqlite" && c.Database.Driver != "postgres" {
		return fmt.Errorf("database.driver 仅支持 sqlite / postgres，收到 %q", c.Database.Driver)
	}
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn 不能为空")
	}
	if c.Storage.DataDir == "" {
		return fmt.Errorf("storage.data_dir 不能为空")
	}
	if c.Auth.JWTSecret == "" {
		return fmt.Errorf("auth.jwt_secret 不能为空")
	}
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port 非法: %d", c.Server.Port)
	}
	if c.Sync.DeviceStaleDays < 0 ||
		c.Sync.ReconcileIntervalHours < 0 ||
		c.Sync.TempFileMaxAgeHours < 0 ||
		c.Sync.OrphanFileGraceHours < 0 {
		return fmt.Errorf("sync maintenance intervals cannot be negative")
	}
	return nil
}

func (c SyncConfig) EffectiveDeviceStaleDays() int {
	if c.DeviceStaleDays <= 0 {
		return 90
	}
	return c.DeviceStaleDays
}

func (c SyncConfig) EffectiveReconcileIntervalHours() int {
	if c.ReconcileIntervalHours <= 0 {
		return 24
	}
	return c.ReconcileIntervalHours
}

func (c SyncConfig) EffectiveTempFileMaxAgeHours() int {
	if c.TempFileMaxAgeHours <= 0 {
		return 24
	}
	return c.TempFileMaxAgeHours
}

func (c SyncConfig) EffectiveOrphanFileGraceHours() int {
	if c.OrphanFileGraceHours <= 0 {
		return 24
	}
	return c.OrphanFileGraceHours
}

// Env 返回当前生效的环境标识（dev / prod）。
func Env() string {
	e := os.Getenv("OSS_ENV")
	if e == "" {
		return "dev"
	}
	return e
}
