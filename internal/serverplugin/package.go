// Package serverplugin 管理服务端插件包、进程、路由和宿主服务调用

package serverplugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"runtime"
	"strings"

	"github.com/helantianshen/oss-sync/internal/blog"
)

const (
	CurrentAPIVersion    = 1
	MaxManifestBytes     = 64 << 10
	MaxArchiveBytes      = 32 << 20
	MaxWasmBytes         = 8 << 20
	MaxPluginMemoryPages = 256
	MaxRequestBytes      = 1 << 20
	MaxResponseBytes     = 1 << 20
	MaxRoutes            = 32
	MaxPluginFiles       = 512
	MaxPluginFileBytes   = 32 << 20
	MaxExtractedBytes    = 64 << 20
	RuntimeWASM          = "wasm"
	RuntimeExecutable    = "executable"
)

var (
	ErrInvalidManifest = errors.New("invalid server plugin manifest")
	ErrInvalidPackage  = errors.New("invalid server plugin package")
	ErrPluginExists    = errors.New("server plugin already exists")
	ErrPluginNotFound  = errors.New("server plugin not found")
	ErrPluginEnabled   = errors.New("server plugin is enabled")
	ErrPluginDisabled  = errors.New("server plugin is disabled")
)

var pluginIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)
var routePathPattern = regexp.MustCompile(`^/(?:[A-Za-z0-9._~-]+(?:/[A-Za-z0-9._~-]+)*)?$`)

// Manifest 是单个服务端插件包受信的解析后元数据
type Manifest struct {
	ID            string                   `json:"id"`
	Name          string                   `json:"name"`
	Version       string                   `json:"version"`
	Description   string                   `json:"description,omitempty"`
	APIVersion    int                      `json:"api_version"`
	Routes        []RouteSpec              `json:"routes"`
	Settings      []blog.ThemeSettingField `json:"settings,omitempty"`
	Hooks         []HookSpec               `json:"hooks,omitempty"`
	Registration  *ExtensionRegistration   `json:"registration,omitempty"`
	Runtime       string                   `json:"runtime,omitempty"`
	Entrypoints   map[string]string        `json:"entrypoints,omitempty"`
	Args          []string                 `json:"args,omitempty"`
	BlogThemes    []ThemeResource          `json:"blog_themes,omitempty"`
	ConsoleThemes []ThemeResource          `json:"console_themes,omitempty"`
	// SettingsVisibility 声明设置入口在侧边栏的可见范围，空值按主题类资源是否被使用推导
	SettingsVisibility string `json:"settings_visibility,omitempty"`
	// UpdateURL 是作者提供的版本查询接口，返回 {"version":"x.y.z","url":"...","notes":"..."}
	// 缺省时宿主不提供更新检查，作者需自行在插件内实现
	UpdateURL string `json:"update_url,omitempty"`
	// AutoCheckUpdate 声明宿主是否自动检查该插件的更新，缺省 false
	AutoCheckUpdate bool `json:"auto_check_update,omitempty"`
}

// 设置入口可见范围取值
const (
	// SettingsVisibilityAlways 启用即显示，不依赖模板或主题是否被选用
	SettingsVisibilityAlways = "always"
	// SettingsVisibilityWhenUsed 仅当插件提供的模板或主题被某个仓库选用时显示
	SettingsVisibilityWhenUsed = "when_used"
)

// SettingsVisibleWhenUsed 判断设置入口是否应跟随模板与主题的实际使用情况
// 未声明时默认 when_used：模板与插件资源只有被选用才出现对应设置项
func (m Manifest) SettingsVisibleWhenUsed() bool {
	return m.SettingsVisibility != SettingsVisibilityAlways
}

// HasThemeResources 判断插件是否提供博客或控制台主题资源
func (m Manifest) HasThemeResources() bool {
	return len(m.BlogThemes) > 0 || len(m.ConsoleThemes) > 0
}

// ThemeResource 声明插件包中的主题资源
type ThemeResource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

func (resource ThemeResource) Key(pluginID string) string {
	return pluginID + "--" + resource.ID
}

// HookSpec 声明受信插件处理的宿主集成点
type HookSpec struct {
	Name  string `json:"name"`
	ID    string `json:"id,omitempty"`
	Label string `json:"label,omitempty"`
}

// RouteSpec 声明插件固定命名空间内的一条路由
type RouteSpec struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Public bool   `json:"public"`
}

// PluginRequest 是暴露给服务端插件的请求数据
type PluginRequest struct {
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	Callback   string              `json:"callback,omitempty"`
	Query      map[string][]string `json:"query,omitempty"`
	Params     map[string]string   `json:"params,omitempty"`
	Headers    map[string][]string `json:"headers,omitempty"`
	Cookies    map[string]string   `json:"cookies,omitempty"`
	User       *PluginUser         `json:"user,omitempty"`
	Settings   map[string]any      `json:"settings,omitempty"`
	Hook       string              `json:"hook,omitempty"`
	Payload    map[string]any      `json:"payload,omitempty"`
	BodyBase64 string              `json:"body_base64,omitempty"`
}

// PluginUser 是插件请求中的已认证用户
type PluginUser struct {
	ID       uint   `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

// PluginResponse 是服务端插件可返回的响应形态
type PluginResponse struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers,omitempty"`
	BodyBase64 string            `json:"body_base64,omitempty"`
}

// ParseManifest 解析并校验插件包清单
func ParseManifest(raw []byte) (Manifest, error) {
	if len(raw) == 0 || len(raw) > MaxManifestBytes {
		return Manifest{}, fmt.Errorf("%w: manifest size is invalid", ErrInvalidManifest)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Manifest{}, fmt.Errorf("%w: manifest must contain one JSON object", ErrInvalidManifest)
		}
		return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	if err := ValidateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// ValidateManifest 校验插件清单的字段与资源约束
func ValidateManifest(manifest Manifest) error {
	if !pluginIDPattern.MatchString(manifest.ID) {
		return fmt.Errorf("%w: id must be 2-64 lowercase letters, digits, or hyphens", ErrInvalidManifest)
	}
	if !boundedText(manifest.Name, 1, 128) {
		return fmt.Errorf("%w: name length is invalid", ErrInvalidManifest)
	}
	if !boundedText(manifest.Version, 1, 64) {
		return fmt.Errorf("%w: version length is invalid", ErrInvalidManifest)
	}
	if len(manifest.Description) > 2000 {
		return fmt.Errorf("%w: description is too long", ErrInvalidManifest)
	}
	if manifest.SettingsVisibility != "" && manifest.SettingsVisibility != SettingsVisibilityAlways && manifest.SettingsVisibility != SettingsVisibilityWhenUsed {
		return fmt.Errorf("%w: unsupported settings_visibility %q", ErrInvalidManifest, manifest.SettingsVisibility)
	}
	if manifest.UpdateURL != "" && !validPluginUpdateURL(manifest.UpdateURL) {
		return fmt.Errorf("%w: update_url must use https or loopback http", ErrInvalidManifest)
	}
	if manifest.APIVersion != CurrentAPIVersion {
		return fmt.Errorf("%w: unsupported api_version %d", ErrInvalidManifest, manifest.APIVersion)
	}
	if len(manifest.Routes) > MaxRoutes {
		return fmt.Errorf("%w: route count is invalid", ErrInvalidManifest)
	}
	if manifestRuntime(manifest) == RuntimeWASM && len(manifest.Routes) == 0 && len(manifest.Hooks) == 0 && len(manifest.Settings) == 0 {
		return fmt.Errorf("%w: WASM plugin must declare a route, hook, or setting", ErrInvalidManifest)
	}
	if err := validateRuntime(manifest); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	if err := blog.ValidateSettingFields(manifest.Settings); err != nil {
		return fmt.Errorf("%w: plugin settings: %v", ErrInvalidManifest, err)
	}
	if err := validateHooks(manifest.Hooks, manifestRuntime(manifest) == RuntimeExecutable); err != nil {
		return fmt.Errorf("%w: plugin hooks: %v", ErrInvalidManifest, err)
	}
	if err := validateThemeResources(manifest.BlogThemes); err != nil {
		return fmt.Errorf("%w: blog themes: %v", ErrInvalidManifest, err)
	}
	if err := validateThemeResources(manifest.ConsoleThemes); err != nil {
		return fmt.Errorf("%w: console themes: %v", ErrInvalidManifest, err)
	}
	seen := make(map[string]struct{}, len(manifest.Routes))
	for _, route := range manifest.Routes {
		if !validMethod(route.Method) {
			return fmt.Errorf("%w: unsupported route method %q", ErrInvalidManifest, route.Method)
		}
		if !validRoutePath(route.Path) {
			return fmt.Errorf("%w: unsafe route path %q", ErrInvalidManifest, route.Path)
		}
		key := route.Method + " " + route.Path
		if _, exists := seen[key]; exists {
			return fmt.Errorf("%w: duplicate route %q", ErrInvalidManifest, key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

var themeResourceIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func validateThemeResources(resources []ThemeResource) error {
	if len(resources) > 32 {
		return errors.New("theme count is invalid")
	}
	seenIDs := make(map[string]struct{}, len(resources))
	seenPaths := make(map[string]struct{}, len(resources))
	for _, resource := range resources {
		if !themeResourceIDPattern.MatchString(resource.ID) || !boundedText(resource.Name, 1, 128) || !validResourceDirectory(resource.Path) {
			return fmt.Errorf("invalid theme resource %q", resource.ID)
		}
		if _, exists := seenIDs[resource.ID]; exists {
			return fmt.Errorf("duplicate theme resource id %q", resource.ID)
		}
		if _, exists := seenPaths[resource.Path]; exists {
			return fmt.Errorf("duplicate theme resource path %q", resource.Path)
		}
		seenIDs[resource.ID] = struct{}{}
		seenPaths[resource.Path] = struct{}{}
	}
	return nil
}

func validResourceDirectory(value string) bool {
	if value == "" || strings.Contains(value, "\\") || strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "/") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func manifestRuntime(manifest Manifest) string {
	if manifest.Runtime == "" {
		return RuntimeWASM
	}
	return manifest.Runtime
}

func manifestEntrypoint(manifest Manifest) (string, error) {
	if manifestRuntime(manifest) != RuntimeExecutable {
		return "plugin.wasm", nil
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	entrypoint := manifest.Entrypoints[platform]
	if entrypoint == "" {
		entrypoint = manifest.Entrypoints["any"]
	}
	if entrypoint == "" {
		return "", fmt.Errorf("no executable entrypoint for %s", platform)
	}
	return entrypoint, nil
}

func validateHooks(hooks []HookSpec, dynamic bool) error {
	if len(hooks) > 32 {
		return errors.New("hook count is invalid")
	}
	seen := make(map[string]struct{}, len(hooks))
	for _, hook := range hooks {
		if dynamic && !validExtensionName(hook.Name) || !dynamic && !validHookName(hook.Name) {
			return fmt.Errorf("unsupported hook %q", hook.Name)
		}
		if hook.Name == "editor.command" && (!pluginIDPattern.MatchString(hook.ID) || !boundedText(hook.Label, 1, 128)) {
			return errors.New("editor command id or label is invalid")
		}
		if _, exists := seen[hook.Name]; exists {
			return fmt.Errorf("duplicate hook %q", hook.Name)
		}
		seen[hook.Name] = struct{}{}
	}
	return nil
}

func validHookName(name string) bool {
	switch name {
	case "blog.content", "markdown.content", "theme.render", "blog.data", "admin.page", "editor.command", "comment.content":
		return true
	default:
		return false
	}
}

func validMethod(method string) bool {
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
		return true
	default:
		return false
	}
}

func validRoutePath(routePath string) bool {
	return routePathPattern.MatchString(routePath) &&
		routePath == cleanRoutePath(routePath) &&
		!strings.Contains(routePath, "//")
}

func cleanRoutePath(routePath string) string {
	parts := strings.Split(routePath, "/")
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			if part == "" && len(cleaned) == 0 {
				cleaned = append(cleaned, "")
			}
		case "..":
			return ""
		default:
			cleaned = append(cleaned, part)
		}
	}
	return strings.Join(cleaned, "/")
}

func boundedText(value string, min, max int) bool {
	trimmed := strings.TrimSpace(value)
	return trimmed == value && len(trimmed) >= min && len(trimmed) <= max
}
