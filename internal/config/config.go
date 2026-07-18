package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 是 OSS 后端的全局配置，按 OSS_ENV 切换配置文件加载。
// 决策 8.1：脚手架必须支持按 OSS_ENV 切换 dev / prod 配置。
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
	MaxMultipartMemoryMB int64 `yaml:"max_multipart_memory_mb"`
	MaxFileSizeMB        int64 `yaml:"max_file_size_mb"`
}

type DatabaseConfig struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

type StorageConfig struct {
	DataDir string `yaml:"data_dir"`
}

type AuthConfig struct {
	JWTSecret   string `yaml:"jwt_secret"`
	JWTTTLHours int    `yaml:"jwt_ttl_hours"`
}

type SyncConfig struct {
	MaxConcurrency int `yaml:"max_concurrency"`
}

// Load 读取与 OSS_ENV 对应的配置文件并合并环境变量覆盖。
//
// OSS_ENV 取值：dev（默认）/ prod。对应 configs/config.<env>.yaml。
// 配置文件查找路径：configs/config.<env>.yaml（相对于工作目录）。
// 敏感字段支持环境变量覆盖，便于 docker-compose / k8s 注入：
//   - OSS_JWT_SECRET
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

	c.applyEnvOverrides()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("OSS_JWT_SECRET"); v != "" {
		c.Auth.JWTSecret = v
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
	return nil
}

// Env 返回当前生效的环境标识（dev / prod）。
func Env() string {
	e := os.Getenv("OSS_ENV")
	if e == "" {
		return "dev"
	}
	return e
}
