// Package config 提供服务端配置的加载、覆盖与校验
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是按 OSS_ENV 选定的有效运行时配置
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Storage  StorageConfig  `yaml:"storage"`
	Auth     AuthConfig     `yaml:"auth"`
	Sync     SyncConfig     `yaml:"sync"`
	Update   UpdateConfig   `yaml:"update"`
}

// ServerConfig 定义 HTTP 服务监听与请求限制
type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// PublicURL 固定公开网站地图使用的站点地址，留空时根据请求确定。
	PublicURL            string `yaml:"public_url,omitempty"`
	Mode                 string `yaml:"mode"`
	MaxMultipartMemoryMB int64  `yaml:"max_multipart_memory_mb"`
	MaxFileSizeMB        int64  `yaml:"max_file_size_mb"`
}

// DatabaseConfig 定义数据库驱动与连接字符串
type DatabaseConfig struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

// StorageConfig 定义正文目录与总存储上限
type StorageConfig struct {
	DataDir        string `yaml:"data_dir"`
	MaxTotalSizeMB int64  `yaml:"max_total_size_mb"`
}

// AuthConfig 定义令牌有效期与注册策略
type AuthConfig struct {
	JWTSecret          string `yaml:"jwt_secret"`
	JWTTTLHours        int    `yaml:"jwt_ttl_hours"`
	WebSessionTTLHours int    `yaml:"web_session_ttl_hours"`
	DeviceJWTTTLHours  int    `yaml:"device_jwt_ttl_hours"`
	// AllowAnonymousRegistration 仅用于初始化新数据库，已有数据库以 SystemSetting 落盘值为准
	AllowAnonymousRegistration bool `yaml:"allow_anonymous_registration"`
}

// SyncConfig 定义同步并发、设备清理和存储对账周期
type SyncConfig struct {
	MaxConcurrency         int `yaml:"max_concurrency"`
	DeviceStaleDays        int `yaml:"device_stale_days"`
	ReconcileIntervalHours int `yaml:"reconcile_interval_hours"`
	TempFileMaxAgeHours    int `yaml:"temp_file_max_age_hours"`
	OrphanFileGraceHours   int `yaml:"orphan_file_grace_hours"`
	// AllowInsecureQueryToken 允许查询参数携带 JWT，用于反代在内网以明文 http 转发 SSE
	AllowInsecureQueryToken bool `yaml:"allow_insecure_query_token"`
}

// UpdateConfig 控制管理员触发的版本检查与更新
type UpdateConfig struct {
	// GitHubRepo 为 owner/repo 形式的发布仓库
	GitHubRepo string `yaml:"github_repo"`
	// DownloadSource 选择 official、proxy 或 custom 下载源
	DownloadSource string `yaml:"download_source"`
	// DownloadProxy 为 custom 源使用的 HTTPS 前缀
	DownloadProxy string `yaml:"download_proxy"`
	// TimeoutSeconds 为 GitHub 请求超时（秒），0 表示使用 15 秒
	TimeoutSeconds int `yaml:"timeout_seconds"`
	// UpdateTimeoutSeconds 为整次更新超时（秒），0 表示使用 600 秒
	UpdateTimeoutSeconds int `yaml:"update_timeout_seconds"`
	// CheckTTLSeconds 控制版本检查结果缓存（秒），0 表示使用 3600 秒
	CheckTTLSeconds int `yaml:"check_ttl_seconds"`
	// CheckLimit 控制版本检查次数，0 表示使用 6 次
	CheckLimit int `yaml:"check_limit"`
	// CheckWindowSeconds 控制检查限流窗口（秒），0 表示使用 60 秒
	CheckWindowSeconds int `yaml:"check_window_seconds"`
}

// Load 按 OSS_ENV 读取配置文件并应用环境变量覆盖
// OSS_ENV 仅接受 dev（默认）或 prod，对应文件为 configs/config.<env>.yaml
// 支持覆盖数据库、服务端、存储、认证与更新字段
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
	if v, ok := os.LookupEnv("OSS_SERVER_PUBLIC_URL"); ok {
		c.Server.PublicURL = v
	}
	if v := os.Getenv("OSS_STORAGE_DIR"); v != "" {
		c.Storage.DataDir = v
	}
	if v := os.Getenv("OSS_STORAGE_MAX_TOTAL_SIZE_MB"); v != "" {
		maxMB, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("OSS_STORAGE_MAX_TOTAL_SIZE_MB 必须是整数，收到 %q", v)
		}
		c.Storage.MaxTotalSizeMB = maxMB
	}
	if v := os.Getenv("OSS_WEB_SESSION_TTL_HOURS"); v != "" {
		hours, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("OSS_WEB_SESSION_TTL_HOURS 必须是整数，收到 %q", v)
		}
		c.Auth.WebSessionTTLHours = hours
	}
	if v := os.Getenv("OSS_DEVICE_JWT_TTL_HOURS"); v != "" {
		hours, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("OSS_DEVICE_JWT_TTL_HOURS 必须是整数，收到 %q", v)
		}
		c.Auth.DeviceJWTTTLHours = hours
	}
	if v := os.Getenv("OSS_DEVICE_STALE_DAYS"); v != "" {
		if days, err := strconv.Atoi(v); err == nil {
			c.Sync.DeviceStaleDays = days
		}
	}
	if v, ok := os.LookupEnv("OSS_ALLOW_INSECURE_QUERY_TOKEN"); ok && v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true":
			c.Sync.AllowInsecureQueryToken = true
		case "false":
			c.Sync.AllowInsecureQueryToken = false
		default:
			return fmt.Errorf(
				"OSS_ALLOW_INSECURE_QUERY_TOKEN 必须为 true 或 false，收到 %q",
				v,
			)
		}
	}
	if v := os.Getenv("OSS_RECONCILE_INTERVAL_HOURS"); v != "" {
		if hours, err := strconv.Atoi(v); err == nil {
			c.Sync.ReconcileIntervalHours = hours
		}
	}
	if v := os.Getenv("OSS_UPDATE_GITHUB_REPO"); v != "" {
		c.Update.GitHubRepo = strings.TrimSpace(v)
	}
	if v := os.Getenv("OSS_UPDATE_DOWNLOAD_SOURCE"); v != "" {
		c.Update.DownloadSource = strings.TrimSpace(v)
	}
	if v := os.Getenv("OSS_UPDATE_DOWNLOAD_PROXY"); v != "" {
		c.Update.DownloadProxy = strings.TrimSpace(v)
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
	if c.Storage.MaxTotalSizeMB < 0 || c.Storage.MaxTotalSizeMB > (1<<42)>>20 {
		return fmt.Errorf("storage.max_total_size_mb 必须在 0..4194304 之间，收到 %d", c.Storage.MaxTotalSizeMB)
	}
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port 非法: %d", c.Server.Port)
	}
	c.Server.PublicURL = strings.TrimSpace(c.Server.PublicURL)
	if c.Server.PublicURL != "" {
		u, err := url.Parse(c.Server.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
			u.User != nil || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" ||
			(u.EscapedPath() != "" && u.EscapedPath() != "/") {
			return fmt.Errorf("server.public_url 必须为不含账号、路径、查询参数和片段的 HTTP 或 HTTPS 站点地址")
		}
		c.Server.PublicURL = u.Scheme + "://" + u.Host
	}
	if c.Sync.DeviceStaleDays < 0 ||
		c.Sync.ReconcileIntervalHours < 0 ||
		c.Sync.TempFileMaxAgeHours < 0 ||
		c.Sync.OrphanFileGraceHours < 0 {
		return fmt.Errorf("sync maintenance intervals cannot be negative")
	}
	if err := c.Auth.validate(); err != nil {
		return err
	}
	if err := c.Update.validate(); err != nil {
		return err
	}
	return nil
}

// MaxTotalSizeBytes 返回整个数据目录的应用层容量上限；0 表示不限
func (c StorageConfig) MaxTotalSizeBytes() int64 {
	return c.MaxTotalSizeMB << 20
}

func (c UpdateConfig) validate() error {
	if s := strings.TrimSpace(c.GitHubRepo); s != "" {
		if err := validateGitHubRepo(s); err != nil {
			return err
		}
	}
	source := strings.TrimSpace(c.DownloadSource)
	if source != "" && source != "official" && source != "proxy" && source != "custom" {
		return fmt.Errorf("update.download_source 仅支持 official / proxy / custom，收到 %q", source)
	}
	proxy := strings.TrimSpace(c.DownloadProxy)
	if proxy != "" {
		if err := validateHTTPSPrefix(proxy); err != nil {
			return fmt.Errorf("update.download_proxy: %w", err)
		}
	}
	if source == "custom" && proxy == "" {
		return fmt.Errorf("update.download_proxy 在 custom 源下不能为空")
	}
	if c.TimeoutSeconds != 0 && (c.TimeoutSeconds < 5 || c.TimeoutSeconds > 120) {
		return fmt.Errorf("update.timeout_seconds 必须在 5..120 之间，收到 %d", c.TimeoutSeconds)
	}
	if c.UpdateTimeoutSeconds != 0 && (c.UpdateTimeoutSeconds < 30 || c.UpdateTimeoutSeconds > 1800) {
		return fmt.Errorf("update.update_timeout_seconds 必须在 30..1800 之间，收到 %d", c.UpdateTimeoutSeconds)
	}
	if c.CheckTTLSeconds != 0 && (c.CheckTTLSeconds < 60 || c.CheckTTLSeconds > 86400) {
		return fmt.Errorf("update.check_ttl_seconds 必须在 60..86400 之间，收到 %d", c.CheckTTLSeconds)
	}
	if c.CheckLimit != 0 && (c.CheckLimit < 1 || c.CheckLimit > 100) {
		return fmt.Errorf("update.check_limit 必须在 1..100 之间，收到 %d", c.CheckLimit)
	}
	if c.CheckWindowSeconds != 0 && (c.CheckWindowSeconds < 10 || c.CheckWindowSeconds > 3600) {
		return fmt.Errorf("update.check_window_seconds 必须在 10..3600 之间，收到 %d", c.CheckWindowSeconds)
	}
	return nil
}

func validateHTTPSPrefix(raw string) error {
	if len(raw) > 1024 {
		return fmt.Errorf("必须不超过 1024 个字符")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("必须为不含账号、查询参数和片段的 HTTPS 地址前缀")
	}
	return nil
}

func validateGitHubRepo(s string) error {
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return fmt.Errorf("update.github_repo 必须为 owner/repo 格式，收到 %q", s)
	}
	owner, repo := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if owner == "" || repo == "" {
		return fmt.Errorf("update.github_repo owner 与 repo 均不能为空，收到 %q", s)
	}
	if !isValidRepoPart(owner) || !isValidRepoPart(repo) {
		return fmt.Errorf("update.github_repo 仅允许字母、数字、- _ . 字符，收到 %q", s)
	}
	return nil
}

func isValidRepoPart(p string) bool {
	if len(p) == 0 || len(p) > 100 {
		return false
	}
	for _, c := range p {
		if (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' {
			continue
		}
		return false
	}
	// 不允许以 . 或 - 开头/结尾
	if p[0] == '.' || p[0] == '-' || p[len(p)-1] == '.' || p[len(p)-1] == '-' {
		return false
	}
	return true
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

// EffectiveGitHubRepo 返回发布仓库，默认指向项目上游仓库
func (c UpdateConfig) EffectiveGitHubRepo() string {
	if s := strings.TrimSpace(c.GitHubRepo); s != "" {
		return s
	}
	return "helantianshen/oss-sync"
}

// EffectiveDownloadSource 返回更新检查与下载使用的源
func (c UpdateConfig) EffectiveDownloadSource() string {
	if source := strings.TrimSpace(c.DownloadSource); source != "" {
		return source
	}
	return "official"
}

// EffectiveDownloadProxy 返回自定义更新源前缀
func (c UpdateConfig) EffectiveDownloadProxy() string {
	return strings.TrimSpace(c.DownloadProxy)
}

// EffectiveTimeout 返回 GitHub 请求超时，边界 5..120，默认 15
func (c UpdateConfig) EffectiveTimeout() time.Duration {
	if c.TimeoutSeconds >= 5 && c.TimeoutSeconds <= 120 {
		return time.Duration(c.TimeoutSeconds) * time.Second
	}
	return 15 * time.Second
}

// EffectiveUpdateTimeout 返回整次更新流程超时，边界 30..1800，默认 600（10 分钟）
func (c UpdateConfig) EffectiveUpdateTimeout() time.Duration {
	if c.UpdateTimeoutSeconds >= 30 && c.UpdateTimeoutSeconds <= 1800 {
		return time.Duration(c.UpdateTimeoutSeconds) * time.Second
	}
	return 600 * time.Second
}

// EffectiveCheckTTL 返回检查结果缓存 TTL，边界 60..86400，默认 3600（1 小时）
func (c UpdateConfig) EffectiveCheckTTL() time.Duration {
	if c.CheckTTLSeconds >= 60 && c.CheckTTLSeconds <= 86400 {
		return time.Duration(c.CheckTTLSeconds) * time.Second
	}
	return 3600 * time.Second
}

// EffectiveCheckLimit 返回检查更新接口的限流次数，边界 1..100，默认 6
func (c UpdateConfig) EffectiveCheckLimit() int {
	if c.CheckLimit >= 1 && c.CheckLimit <= 100 {
		return c.CheckLimit
	}
	return 6
}

// EffectiveCheckWindow 返回检查更新接口的限流窗口，边界 10..3600，默认 60
func (c UpdateConfig) EffectiveCheckWindow() time.Duration {
	if c.CheckWindowSeconds >= 10 && c.CheckWindowSeconds <= 3600 {
		return time.Duration(c.CheckWindowSeconds) * time.Second
	}
	return time.Minute
}

// Env 返回当前生效的环境标识（dev / prod）
func Env() string {
	e := os.Getenv("OSS_ENV")
	if e == "" {
		return "dev"
	}
	return e
}

// SaveDatabaseConfig 将数据库启动配置写入当前环境对应的 YAML 文件
// SaveDatabaseConfig 不改变当前进程已建立的数据库连接
func SaveDatabaseConfig(driver, dsn string) error {
	driver = strings.ToLower(strings.TrimSpace(driver))
	dsn = strings.TrimSpace(dsn)
	if driver != "sqlite" && driver != "postgres" {
		return fmt.Errorf("database.driver 仅支持 sqlite / postgres，收到 %q", driver)
	}
	if dsn == "" {
		return fmt.Errorf("database.dsn 不能为空")
	}
	path, err := configPath()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}
	var stored Config
	if err := yaml.Unmarshal(raw, &stored); err != nil {
		return fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	stored.Database = DatabaseConfig{Driver: driver, DSN: dsn}
	if err := stored.validate(); err != nil {
		return err
	}
	updated, err := yaml.Marshal(&stored)
	if err != nil {
		return fmt.Errorf("序列化配置文件 %s 失败: %w", path, err)
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		return fmt.Errorf("写入配置文件 %s 失败: %w", path, err)
	}
	return nil
}

func configPath() (string, error) {
	env := Env()
	if env != "dev" && env != "prod" {
		return "", fmt.Errorf("OSS_ENV 仅支持 dev / prod，收到 %q", env)
	}
	return filepath.Join("configs", fmt.Sprintf("config.%s.yaml", env)), nil
}
