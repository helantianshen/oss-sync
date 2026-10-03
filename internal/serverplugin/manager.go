package serverplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/blog"
	"github.com/helantianshen/oss-sync/internal/models"
	"github.com/helantianshen/oss-sync/internal/version"
)

// PluginInfo 是已安装服务端插件面向管理面的安全视图
type PluginInfo struct {
	Manifest
	Enabled      bool
	LastError    string
	WasmHash     string
	ManifestHash string
	WasmSize     int64
	PayloadHash  string
	PayloadSize  int64
	// InstalledBytes 是插件目录在磁盘上的实际占用，含运行期产生的数据
	InstalledBytes     int64
	InstalledAt        time.Time
	UpdatedAt          time.Time
	Associations       []PluginAssociation
	AssociationSummary string
	Builtin            bool
}

// PluginAssociation 表示插件与资源的关联
type PluginAssociation struct {
	Kind       string
	TargetID   string
	TargetName string
}

// ThemeOption 表示可选的主题资源
type ThemeOption struct {
	Name               string
	Label              string
	PluginID           string
	Builtin            bool
	SupportsPublicBlog bool
}

// Manager 持有已安装插件文件与已启用的编译模块
type Manager struct {
	db      *gorm.DB
	root    string
	runtime *Runtime

	mu            sync.RWMutex
	lifecycleMu   sync.Mutex
	modules       map[string]pluginInstance
	registrations map[string]ExtensionRegistration
	scheduler     pluginTaskScheduler
	fileWriter    FileWriter
}

// FileWriter 由同步层实现，供插件通过 host.file.put 复用真实的文件写入管线
// 写入在服务器进程内完成，从而与客户端同步共享同一套并发锁与修订通知
type FileWriter interface {
	WriteFileContent(userID uint, vaultID, path string, content []byte, mtime int64) (models.File, error)
}

// SetFileWriter 注入文件写入实现，未注入时 host.file.put 返回不可用错误
func (m *Manager) SetFileWriter(writer FileWriter) {
	m.fileWriter = writer
}

type pluginInstance interface {
	Invoke(context.Context, PluginRequest) (PluginResponse, error)
	Close(context.Context) error
	Healthy() bool
	Registration() ExtensionRegistration
}

type wasmPluginInstance struct {
	runtime      *Runtime
	module       wazero.CompiledModule
	registration ExtensionRegistration
}

func (p *wasmPluginInstance) Invoke(ctx context.Context, request PluginRequest) (PluginResponse, error) {
	return p.runtime.Invoke(ctx, p.module, request)
}

func (p *wasmPluginInstance) Close(ctx context.Context) error {
	return p.module.Close(ctx)
}

func (p *wasmPluginInstance) Healthy() bool {
	return true
}

func (p *wasmPluginInstance) Registration() ExtensionRegistration {
	return p.registration
}

// NewManager 加载插件目录与已安装插件状态
func NewManager(ctx context.Context, db *gorm.DB, dataDir string) (*Manager, error) {
	if ctx == nil {
		return nil, errors.New("plugin manager context is nil")
	}
	if db == nil {
		return nil, errors.New("plugin manager database is nil")
	}
	root, err := filepath.Abs(filepath.Join(dataDir, "plugins"))
	if err != nil {
		return nil, fmt.Errorf("resolve plugin root: %w", err)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create plugin root: %w", err)
	}
	runtime, err := NewRuntime(ctx)
	if err != nil {
		return nil, err
	}
	return &Manager{
		db: db, root: root, runtime: runtime,
		modules:       make(map[string]pluginInstance),
		registrations: make(map[string]ExtensionRegistration),
	}, nil
}

func (m *Manager) LoadEnabled(ctx context.Context) error {
	var records []models.ServerPlugin
	if err := m.db.Where("enabled = ?", true).Order("id asc").Find(&records).Error; err != nil {
		return fmt.Errorf("load enabled plugins: %w", err)
	}
	ordered, err := m.orderEnabledRecords(records)
	if err != nil {
		return err
	}
	var loadErrors []error
	for _, record := range ordered {
		if err := m.loadRecord(ctx, record); err != nil {
			loadErrors = append(loadErrors, fmt.Errorf("plugin %s: %w", record.ID, err))
			if saveErr := m.saveLastError(record.ID, err); saveErr != nil {
				loadErrors = append(loadErrors, fmt.Errorf("plugin %s: save load error: %w", record.ID, saveErr))
			}
		}
	}
	return errors.Join(loadErrors...)
}

func (m *Manager) orderEnabledRecords(records []models.ServerPlugin) ([]models.ServerPlugin, error) {
	byID := make(map[string]models.ServerPlugin, len(records))
	dependencies := make(map[string][]string, len(records))
	for _, record := range records {
		byID[record.ID] = record
		manifest, err := ParseManifest([]byte(record.ManifestJSON))
		if err != nil {
			return nil, fmt.Errorf("decode plugin %s manifest: %w", record.ID, err)
		}
		for _, dependency := range registrationFromManifest(manifest).Dependencies {
			dependencies[record.ID] = append(dependencies[record.ID], dependency.PluginID)
		}
	}
	state := make(map[string]uint8, len(records))
	ordered := make([]models.ServerPlugin, 0, len(records))
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("plugin dependency cycle includes %s", id)
		case 2:
			return nil
		}
		state[id] = 1
		for _, dependencyID := range dependencies[id] {
			if _, exists := byID[dependencyID]; !exists {
				continue
			}
			if err := visit(dependencyID); err != nil {
				return err
			}
		}
		state[id] = 2
		ordered = append(ordered, byID[id])
		return nil
	}
	for _, record := range records {
		if err := visit(record.ID); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func (m *Manager) List() ([]PluginInfo, error) {
	var records []models.ServerPlugin
	if err := m.db.Order("id asc").Find(&records).Error; err != nil {
		return nil, fmt.Errorf("list server plugins: %w", err)
	}
	plugins := make([]PluginInfo, 0, len(records))
	for _, record := range records {
		info, err := infoFromRecord(record)
		if err != nil {
			return nil, fmt.Errorf("decode plugin %s: %w", record.ID, err)
		}
		info.Associations = pluginAssociations(m.db, record.ID)
		labels := make([]string, 0, len(info.Associations))
		for _, association := range info.Associations {
			labels = append(labels, association.TargetName)
		}
		info.AssociationSummary = strings.Join(labels, ", ")
		m.mu.RLock()
		registration, registered := m.registrations[record.ID]
		m.mu.RUnlock()
		if registered {
			info.Settings = registration.Settings
			info.Hooks = legacyHookSpecs(registration.Hooks)
		}
		if info.Runtime == "" {
			info.Runtime = RuntimeWASM
		}
		info.Builtin = record.Builtin
		info.InstalledBytes = installedPackageBytes(filepath.Join(m.root, record.ID))
		plugins = append(plugins, info)
	}
	return plugins, nil
}

// installedPackageBytes 统计插件目录在磁盘上的实际占用
// 插件可能在运行期持续写入数据，安装时记录的 payload 大小无法反映真实占用
func installedPackageBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, infoErr := entry.Info(); infoErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// PluginUpdateInfo 是单个插件的可用更新
type PluginUpdateInfo struct {
	PluginID      string `json:"plugin_id"`
	Current       string `json:"current_version"`
	Latest        string `json:"latest_version"`
	DownloadURL   string `json:"download_url,omitempty"`
	Notes         string `json:"notes,omitempty"`
	HasUpdate     bool   `json:"has_update"`
	UpdatableHere bool   `json:"updatable"`
	CheckFailed   bool   `json:"check_failed,omitempty"`
}

func validPluginUpdateURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return true
	case "http":
		host := parsed.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}

func pluginUpdateHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			if !validPluginUpdateURL(req.URL.String()) {
				return errors.New("plugin update redirect uses an unsafe URL")
			}
			return nil
		},
	}
}

// PluginUpdateFromURL 下载作者更新接口提供的 ZIP 并完成升级
// 下载地址由 manifest 的 update_url 接口返回，仅限 https 或本机回环
func (m *Manager) PluginUpdateFromURL(ctx context.Context, pluginID, expectedVersion, downloadURL string) (PluginInfo, error) {
	if !validPluginUpdateURL(downloadURL) {
		return PluginInfo{}, fmt.Errorf("plugin %s provides an unsafe download url", pluginID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return PluginInfo{}, err
	}
	res, err := pluginUpdateHTTPClient(5 * time.Minute).Do(req)
	if err != nil {
		return PluginInfo{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return PluginInfo{}, fmt.Errorf("download plugin %s: status %d", pluginID, res.StatusCode)
	}
	content, err := io.ReadAll(io.LimitReader(res.Body, MaxArchiveBytes+1))
	if err != nil {
		return PluginInfo{}, err
	}
	if len(content) > MaxArchiveBytes {
		return PluginInfo{}, fmt.Errorf("plugin package exceeds %d bytes", MaxArchiveBytes)
	}
	packageData, err := ParsePackage(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return PluginInfo{}, err
	}
	if packageData.Manifest.ID != pluginID {
		return PluginInfo{}, fmt.Errorf("plugin update id %q does not match %q", packageData.Manifest.ID, pluginID)
	}
	if order, compareErr := version.Compare(packageData.Manifest.Version, expectedVersion); compareErr != nil || order != 0 {
		return PluginInfo{}, fmt.Errorf("plugin update version %q does not match %q", packageData.Manifest.Version, expectedVersion)
	}
	// 更新检查只覆盖已启用插件，Upgrade 会保留其启用状态并完成运行时恢复
	return m.Upgrade(ctx, bytes.NewReader(content), int64(len(content)))
}

// CheckPluginUpdates 批量查询已启用插件声明的更新接口
// updateURL 缺省的插件直接跳过：作者可自行在插件内实现更新
func (m *Manager) CheckPluginUpdates(ctx context.Context) ([]PluginUpdateInfo, error) {
	records := []models.ServerPlugin{}
	if err := m.db.Where("enabled = ?", true).Order("id asc").Find(&records).Error; err != nil {
		return nil, fmt.Errorf("list plugins for update check: %w", err)
	}
	results := make([]PluginUpdateInfo, 0, len(records))
	for _, record := range records {
		info, err := infoFromRecord(record)
		if err != nil {
			continue
		}
		if info.UpdateURL == "" {
			continue
		}
		entry := PluginUpdateInfo{PluginID: record.ID, Current: info.Version}
		latest, downloadURL, notes, fetchErr := m.fetchPluginUpdate(ctx, info.UpdateURL)
		if fetchErr != nil {
			// 接口失败不阻塞其他插件的检查，也不写入错误状态
			entry.CheckFailed = true
			results = append(results, entry)
			continue
		}
		entry.Latest = latest
		entry.DownloadURL = downloadURL
		entry.Notes = notes
		entry.HasUpdate = latest != "" && versionGreater(latest, info.Version)
		// 提供下载地址才能由宿主代理更新；否则提示作者自行更新
		entry.UpdatableHere = entry.HasUpdate && downloadURL != ""
		results = append(results, entry)
	}
	return results, nil
}

func (m *Manager) fetchPluginUpdate(ctx context.Context, updateURL string) (version, downloadURL, notes string, err error) {
	if !validPluginUpdateURL(updateURL) {
		return "", "", "", errors.New("plugin update endpoint uses an unsafe URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, updateURL, nil)
	if err != nil {
		return "", "", "", err
	}
	client := pluginUpdateHTTPClient(10 * time.Second)
	res, err := client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("update endpoint status %d", res.StatusCode)
	}
	var payload struct {
		Version     string `json:"version"`
		DownloadURL string `json:"url"`
		Notes       string `json:"notes"`
	}
	body, readErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if readErr != nil {
		return "", "", "", readErr
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", "", err
	}
	if payload.DownloadURL != "" && !validPluginUpdateURL(strings.TrimSpace(payload.DownloadURL)) {
		return "", "", "", errors.New("plugin download uses an unsafe URL")
	}
	return strings.TrimSpace(payload.Version), strings.TrimSpace(payload.DownloadURL), strings.TrimSpace(payload.Notes), nil
}

// versionGreater 按严格 SemVer 比较，非法版本视为不大于
func versionGreater(candidate, current string) bool {
	order, err := version.Compare(candidate, current)
	if err != nil {
		return false
	}
	return order > 0
}

func pluginAssociations(db *gorm.DB, pluginID string) []PluginAssociation {
	var rows []models.ServerPluginAssociation
	if err := db.Where("plugin_id = ?", pluginID).Order("kind asc, target_id asc").Find(&rows).Error; err != nil {
		return []PluginAssociation{}
	}
	associations := make([]PluginAssociation, 0, len(rows))
	for _, row := range rows {
		associations = append(associations, PluginAssociation{Kind: row.Kind, TargetID: row.TargetID, TargetName: row.TargetName})
	}
	return associations
}

func (m *Manager) Install(ctx context.Context, reader io.ReaderAt, size int64) (PluginInfo, error) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	packageData, err := ParsePackage(reader, size)
	if err != nil {
		return PluginInfo{}, err
	}
	if err := m.validatePackage(ctx, packageData); err != nil {
		return PluginInfo{}, err
	}

	var existing models.ServerPlugin
	if err := m.db.Where("id = ?", packageData.Manifest.ID).First(&existing).Error; err == nil {
		return PluginInfo{}, ErrPluginExists
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return PluginInfo{}, fmt.Errorf("check existing plugin: %w", err)
	}
	target, err := writePackage(m.root, packageData)
	if err != nil {
		return PluginInfo{}, err
	}
	if manifestRuntime(packageData.Manifest) == RuntimeExecutable {
		instance, startErr := m.openInstance(ctx, target, packageData)
		if startErr != nil {
			return PluginInfo{}, cleanupInstalledPackage(target, startErr)
		}
		if closeErr := instance.Close(ctx); closeErr != nil {
			return PluginInfo{}, cleanupInstalledPackage(target, fmt.Errorf("close validation plugin: %w", closeErr))
		}
	}
	manifestJSON, err := manifestJSON(packageData.Manifest)
	if err != nil {
		return PluginInfo{}, cleanupInstalledPackage(target, err)
	}
	record := models.ServerPlugin{
		ID:           packageData.Manifest.ID,
		Name:         packageData.Manifest.Name,
		Version:      packageData.Manifest.Version,
		Description:  packageData.Manifest.Description,
		APIVersion:   packageData.Manifest.APIVersion,
		Runtime:      manifestRuntime(packageData.Manifest),
		ManifestJSON: manifestJSON,
		ManifestHash: packageData.ManifestHash,
		WasmHash:     packageData.WasmHash,
		WasmSize:     int64(len(packageData.Wasm)),
		PayloadHash:  packageData.PayloadHash,
		PayloadSize:  packageData.PayloadSize,
		InstalledAt:  time.Now().UTC(),
	}
	if err := m.db.Create(&record).Error; err != nil {
		cleanupErr := os.RemoveAll(target)
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return PluginInfo{}, errors.Join(ErrPluginExists, cleanupError(cleanupErr))
		}
		return PluginInfo{}, errors.Join(fmt.Errorf("save plugin record: %w", err), cleanupError(cleanupErr))
	}
	return infoFromRecord(record)
}

func (m *Manager) EnabledThemeOptions() ([]ThemeOption, []ThemeOption) {
	blogThemes := []ThemeOption{{Name: "default", Label: "default", Builtin: true}, {Name: "papertrail", Label: "papertrail", Builtin: true, SupportsPublicBlog: true}}
	consoleThemes := []ThemeOption{{Name: "default", Label: "default", Builtin: true}}
	var records []models.ServerPlugin
	if err := m.db.Where("enabled = ?", true).Order("id asc").Find(&records).Error; err != nil {
		return blogThemes, consoleThemes
	}
	for _, record := range records {
		manifest, err := ParseManifest([]byte(record.ManifestJSON))
		if err != nil {
			continue
		}
		for _, resource := range manifest.BlogThemes {
			name := resource.Key(manifest.ID)
			blogThemes = append(blogThemes, ThemeOption{Name: name, Label: resource.Name, PluginID: manifest.ID, SupportsPublicBlog: blog.SupportsPublicBlog(filepath.Dir(m.root), name)})
		}
		for _, resource := range manifest.ConsoleThemes {
			consoleThemes = append(consoleThemes, ThemeOption{Name: resource.Key(manifest.ID), Label: resource.Name, PluginID: manifest.ID})
		}
	}
	return blogThemes, consoleThemes
}

// InstallOrReuse 安装插件包，已安装相同包时直接复用
// 多个主题可依赖同一份管理员审核通过的插件
func (m *Manager) InstallOrReuse(ctx context.Context, reader io.ReaderAt, size int64) (PluginInfo, error) {
	packageData, err := ParsePackage(reader, size)
	if err != nil {
		return PluginInfo{}, err
	}
	var existing models.ServerPlugin
	err = m.db.Where("id = ?", packageData.Manifest.ID).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return m.Install(ctx, reader, size)
	}
	if err != nil {
		return PluginInfo{}, fmt.Errorf("check existing plugin: %w", err)
	}
	if !samePackage(existing, packageData) {
		return PluginInfo{}, ErrPluginExists
	}
	return infoFromRecord(existing)
}

// Upgrade 替换已安装的插件包，失败时恢复旧运行时
// 已提交的插件迁移与外部生命周期副作用必须保持向后兼容
func (m *Manager) Upgrade(ctx context.Context, reader io.ReaderAt, size int64) (PluginInfo, error) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	return m.upgrade(ctx, reader, size)
}

func (m *Manager) upgrade(ctx context.Context, reader io.ReaderAt, size int64) (info PluginInfo, resultErr error) {
	packageData, err := ParsePackage(reader, size)
	if err != nil {
		return PluginInfo{}, err
	}
	if err := m.validatePackage(ctx, packageData); err != nil {
		return PluginInfo{}, err
	}
	record, err := m.record(packageData.Manifest.ID)
	if err != nil {
		return PluginInfo{}, err
	}
	if record.Builtin {
		return PluginInfo{}, errors.New("built-in plugin cannot be upgraded")
	}
	if samePackage(record, packageData) {
		return infoFromRecord(record)
	}
	wasEnabled := record.Enabled
	pluginDir := filepath.Join(m.root, record.ID)
	var tombstone string
	var candidate pluginInstance
	committed := false
	defer func() {
		if committed {
			return
		}
		// 已取消的上传请求也不能阻止旧插件恢复
		recoveryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var recoveryErr error
		if candidate != nil {
			recoveryErr = errors.Join(recoveryErr, candidate.Close(recoveryCtx))
		}
		m.mu.Lock()
		current := m.modules[record.ID]
		delete(m.modules, record.ID)
		m.removeRegistration(record.ID)
		scheduler := m.scheduler
		m.mu.Unlock()
		if scheduler != nil {
			scheduler.RemovePluginTasks(record.ID)
		}
		if current != nil {
			recoveryErr = errors.Join(recoveryErr, current.Close(recoveryCtx))
		}
		if tombstone != "" {
			if err := os.RemoveAll(pluginDir); err != nil {
				resultErr = errors.Join(resultErr, recoveryErr, fmt.Errorf("remove failed upgrade: %w", err))
				return
			}
			if err := os.Rename(tombstone, pluginDir); err != nil {
				resultErr = errors.Join(resultErr, recoveryErr, fmt.Errorf("restore old package: %w", err))
				return
			}
		}
		record.Enabled = false
		if err := m.db.WithContext(recoveryCtx).Save(&record).Error; err != nil {
			recoveryErr = errors.Join(recoveryErr, err)
		} else if wasEnabled {
			recoveryErr = errors.Join(recoveryErr, m.enable(recoveryCtx, record.ID))
		}
		if recoveryErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("restore previous plugin: %w", recoveryErr))
		}
	}()
	if wasEnabled {
		if err := m.disable(ctx, record.ID); err != nil {
			return PluginInfo{}, err
		}
	}
	tombstone, err = stagePluginDeletion(m.root, pluginDir)
	if err != nil {
		return PluginInfo{}, err
	}
	target, err := writePackage(m.root, packageData)
	if err != nil {
		return PluginInfo{}, err
	}
	instance, err := m.openInstance(ctx, target, packageData)
	if err != nil {
		return PluginInfo{}, err
	}
	candidate = instance
	registration := instance.Registration()
	if err := validateRegistration(registration); err != nil {
		return PluginInfo{}, err
	}
	if err := m.checkDependencies(registration.Dependencies); err != nil {
		return PluginInfo{}, err
	}
	if err := m.applyMigrations(ctx, record.ID, registration.Migrations); err != nil {
		return PluginInfo{}, err
	}
	if err := invokeInstanceLifecycle(ctx, instance, registration.Lifecycle.Upgrade, "upgrade"); err != nil {
		return PluginInfo{}, err
	}
	if err := instance.Close(ctx); err != nil {
		return PluginInfo{}, err
	}
	candidate = nil
	manifestJSON, err := manifestJSON(packageData.Manifest)
	if err != nil {
		return PluginInfo{}, err
	}
	updates := map[string]any{
		"name": packageData.Manifest.Name, "version": packageData.Manifest.Version,
		"description": packageData.Manifest.Description, "api_version": packageData.Manifest.APIVersion,
		"runtime": manifestRuntime(packageData.Manifest), "manifest_json": manifestJSON,
		"manifest_hash": packageData.ManifestHash, "wasm_hash": packageData.WasmHash,
		"wasm_size": int64(len(packageData.Wasm)), "payload_hash": packageData.PayloadHash,
		"payload_size": packageData.PayloadSize, "last_error": "",
	}
	if err := m.db.Model(&models.ServerPlugin{}).Where("id = ?", record.ID).Updates(updates).Error; err != nil {
		return PluginInfo{}, err
	}
	if wasEnabled {
		if err := m.enable(ctx, record.ID); err != nil {
			return PluginInfo{}, err
		}
	}
	info, err = m.info(record.ID)
	if err != nil {
		return PluginInfo{}, err
	}
	committed = true
	if err := os.RemoveAll(tombstone); err != nil {
		return info, fmt.Errorf("upgrade succeeded but old package cleanup failed: %w", err)
	}
	return info, nil
}

func (m *Manager) info(id string) (PluginInfo, error) {
	record, err := m.record(id)
	if err != nil {
		return PluginInfo{}, err
	}
	return infoFromRecord(record)
}

func (m *Manager) validatePackage(ctx context.Context, packageData Package) error {
	if manifestRuntime(packageData.Manifest) != RuntimeWASM {
		return nil
	}
	compiled, err := m.runtime.Compile(ctx, packageData.Wasm)
	if err != nil {
		return err
	}
	if err := compiled.Close(ctx); err != nil {
		return fmt.Errorf("close validation module: %w", err)
	}
	return nil
}

func (m *Manager) openInstance(ctx context.Context, dir string, packageData Package) (pluginInstance, error) {
	switch manifestRuntime(packageData.Manifest) {
	case RuntimeWASM:
		compiled, err := m.runtime.Compile(ctx, packageData.Wasm)
		if err != nil {
			return nil, err
		}
		return &wasmPluginInstance{
			runtime: m.runtime, module: compiled,
			registration: registrationFromManifest(packageData.Manifest),
		}, nil
	case RuntimeExecutable:
		return startExecutablePlugin(
			ctx,
			dir,
			packageData.Manifest,
			func(callCtx context.Context, method string, params map[string]json.RawMessage) (any, error) {
				return m.hostCall(callCtx, packageData.Manifest.ID, method, params)
			},
		)
	default:
		return nil, fmt.Errorf("unsupported plugin runtime %q", packageData.Manifest.Runtime)
	}
}

func (m *Manager) invokeCallback(
	ctx context.Context,
	pluginID string,
	callback string,
	request PluginRequest,
) (PluginResponse, error) {
	m.mu.RLock()
	instance := m.modules[pluginID]
	m.mu.RUnlock()
	if instance == nil || !instance.Healthy() {
		return PluginResponse{}, fmt.Errorf("plugin %s is unavailable", pluginID)
	}
	request.Callback = callback
	return instance.Invoke(ctx, request)
}

func legacyHookSpecs(hooks []RegisteredHook) []HookSpec {
	result := make([]HookSpec, 0, len(hooks))
	for _, hook := range hooks {
		result = append(result, HookSpec{Name: hook.Name, ID: hook.ID, Label: hook.Label})
	}
	return result
}

func samePackage(record models.ServerPlugin, packageData Package) bool {
	recordRuntime := record.Runtime
	if recordRuntime == "" {
		recordRuntime = RuntimeWASM
	}
	if recordRuntime != manifestRuntime(packageData.Manifest) || record.ManifestHash != packageData.ManifestHash {
		return false
	}
	if record.PayloadHash != "" {
		return record.PayloadHash == packageData.PayloadHash && record.PayloadSize == packageData.PayloadSize
	}
	return record.WasmHash == packageData.WasmHash && record.WasmSize == int64(len(packageData.Wasm))
}
