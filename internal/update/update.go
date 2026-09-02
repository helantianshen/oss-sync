// Package update 提供服务端自动更新能力：对比 GitHub Release、下载并原子替换
// 当前二进制，然后请求进程优雅退出（先停 cron、再 HTTP Shutdown、再关 DB），
// 由 supervisor 以新二进制重新拉起。重启后通过 StartupHealthCheck 自检，
// 未就绪时自动回滚到备份二进制。
//
// 更新只由管理员手动触发，不存在任何后台自动拉取。
package update

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/version"
)

// Options 覆盖 Updater 的默认行为，主要用于测试。
type Options struct {
	// GitHubRepo 覆盖配置中的发布仓库，格式 owner/repo。
	GitHubRepo string
	// GitHubToken 覆盖环境变量 OSS_GITHUB_TOKEN。
	GitHubToken string
	// APIBase 覆盖 GitHub API 地址（默认 https://api.github.com），仅供测试。
	APIBase string
	// DownloadSource 覆盖配置中的更新源：official / proxy / custom。
	DownloadSource string
	// DownloadProxy 覆盖配置中的自定义 HTTPS 地址前缀。
	DownloadProxy string
	// ExecPath 覆盖当前可执行文件路径（默认 os.Executable）。
	ExecPath string
	// HTTPClient 覆盖 GitHub 请求使用的 HTTP 客户端（默认带超时的客户端）。
	HTTPClient *http.Client
	// Verifier 校验下载后的二进制；默认检查可执行格式并运行 --version。
	Verifier func(path, wantVersion string) error
	// Pin 指定必须更新到的目标 tag（可带可不带 v 前缀）。
	Pin string
	// SkipTLSVerify 允许跳过 HTTPS 证书校验，便于本地测试与自签名场景。
	SkipTLSVerify bool
}

// Updater 是更新流程的所有者。同一实例同时只有一次更新在进行。
type Updater struct {
	gh           *GitHubClient
	exe          string
	backup       string
	verifier     func(path, wantVersion string) error
	running      atomic.Bool
	restartFired atomic.Bool

	stateMu     sync.Mutex // 保护 lastCheck / lastUpdate
	lastCheck   *CheckResult
	lastUpdate  *UpdateResult
	updatePhase OperationState
	pinned      string
	source      string
	proxy       string

	onUpdated func()
}

const (
	updatePhaseIdle         = StateIdle
	updatePhasePrepare      = StatePrepare
	updatePhaseFetchRelease = StateFetchRelease
	updatePhaseSelectAsset  = StateSelectAsset
	updatePhaseDownload     = StateDownload
	updatePhaseVerify       = StateVerify
	updatePhaseBackup       = StateBackup
	updatePhaseSwap         = StateSwap
	updatePhaseDone         = StateDone
	updatePhaseFailed       = StateFailed
	updatePhaseUpToDate     = StateUpToDate
)

// NewUpdater 构造更新器。当前二进制路径默认取 os.Executable()。
func NewUpdater(cfg *config.Config, opts ...Options) (*Updater, error) {
	opt := Options{}
	if len(opts) > 0 {
		opt = opts[0]
	}
	httpClient := opt.HTTPClient
	if httpClient == nil {
		httpClient = defaultUpdaterHTTPClient(cfg.Update.EffectiveTimeout(), opt.SkipTLSVerify)
	}
	exe := opt.ExecPath
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("无法确定当前可执行文件路径: %w", err)
		}
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return nil, err
	}
	ghCfg := cfg
	if opt.GitHubRepo != "" {
		ghCfg = cloneWithRepo(cfg, opt.GitHubRepo)
	}
	verifier := opt.Verifier
	if verifier == nil {
		verifier = defaultVerifier
	}
	gh := newGitHubClient(ghCfg, httpClient)
	if opt.APIBase != "" {
		gh.apiBase = opt.APIBase
	}
	source := cfg.Update.EffectiveDownloadSource()
	proxy := cfg.Update.EffectiveDownloadProxy()
	if opt.DownloadSource != "" {
		source = opt.DownloadSource
	}
	if opt.DownloadProxy != "" {
		proxy = opt.DownloadProxy
	}
	return &Updater{
		gh:          gh,
		exe:         abs,
		backup:      abs + ".bak",
		verifier:    verifier,
		pinned:      normalizeVersion(opt.Pin),
		updatePhase: updatePhaseIdle,
		source:      source,
		proxy:       proxy,
	}, nil
}

func defaultUpdaterHTTPClient(timeout time.Duration, skipTLSVerify bool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if skipTLSVerify {
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{}
		}
		tr.TLSClientConfig.InsecureSkipVerify = true
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

func cloneWithRepo(cfg *config.Config, repo string) *config.Config {
	cp := *cfg
	cp.Update = cfg.Update
	cp.Update.GitHubRepo = repo
	return &cp
}

// SetOnUpdated 注册“更新完成，请求优雅重启”的回调，只会被调用一次。
func (u *Updater) SetOnUpdated(fn func()) {
	u.onUpdated = fn
}

// TriggerRestart 在响应写回之后异步触发重启回调，避免阻塞 HTTP 响应。
// 回调至多触发一次。
func (u *Updater) TriggerRestart() {
	if u.onUpdated == nil || !u.restartFired.CompareAndSwap(false, true) {
		return
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		u.onUpdated()
	}()
}

// CheckUpdate 对比当前版本与 GitHub latest release，并记录检查状态。
// 使用 internal/version 严格比较，malformed/prerelease 已在 fetchLatest 层被拒绝；
// 当前为开发版本时视为始终有可用更新（若上游为稳定版本）。
func (u *Updater) CheckUpdate(ctx context.Context) (*CheckResult, error) {
	release, err := u.gh.fetchLatestFrom(ctx, u.source, u.proxy)
	if err != nil && !errors.Is(err, ErrNoRelease) {
		return nil, err
	}
	current := version.Version
	res := &CheckResult{
		CheckedAt:      time.Now().UTC(),
		CurrentVersion: current,
	}
	if err == nil {
		res.LatestVersion = release.TagName
		res.ReleaseURL = release.HTMLURL
		if isReleaseNewerThanCurrent(release.TagName, current) {
			res.UpdateAvailable = true
		}
	} else {
		res.Note = "上游仓库暂无 Release"
	}
	u.stateMu.Lock()
	cp := *res
	u.lastCheck = &cp
	u.stateMu.Unlock()
	// return a copy distinct from the stored one to preserve immutability
	rcp := *res
	return &rcp, nil
}

func isReleaseNewerThanCurrent(releaseTag, current string) bool {
	if version.IsDevelopmentVersion(current) {
		// 任意合法稳定 Release 均视为比 dev 新
		if _, err := version.Parse(releaseTag); err == nil {
			sv, _ := version.Parse(releaseTag)
			if sv.Prerelease == "" {
				return true
			}
		}
		return false
	}
	cmp, err := version.Compare(releaseTag, current)
	if err != nil {
		return false
	}
	return cmp > 0
}

// Update 执行完整更新流程：查询 Release → 选择资产 → 下载校验 → 备份 → 原子替换。
// 成功后由调用方决定何时 TriggerRestart。
func (u *Updater) Update(ctx context.Context) *UpdateResult {
	if !u.running.CompareAndSwap(false, true) {
		return &UpdateResult{
			At:    time.Now().UTC(),
			Code:  "in_progress",
			Phase: StateInProgress,
			State: StateInProgress,
			Error: "更新已在进行中",
		}
	}
	defer u.running.Store(false)

	res := &UpdateResult{At: time.Now().UTC()}
	u.setUpdateResultPhase(res, updatePhasePrepare)

	release, err := u.gh.fetchLatestFrom(ctx, u.source, u.proxy)
	u.setUpdateResultPhase(res, updatePhaseFetchRelease)
	if err != nil {
		u.setUpdateResultPhase(res, updatePhaseFailed)
		res.Code = "github_error"
		res.Error = err.Error()
		return res
	}
	sv, err := version.Parse(release.TagName)
	if err != nil {
		u.setUpdateResultPhase(res, updatePhaseFailed)
		res.Code = "github_error"
		res.Error = fmt.Sprintf("Release tag 非法 %q: %v", release.TagName, err)
		return res
	}
	if sv.Prerelease != "" {
		u.setUpdateResultPhase(res, updatePhaseFailed)
		res.Code = "github_error"
		res.Error = fmt.Sprintf("Release tag %q 为 prerelease，不允许", release.TagName)
		return res
	}
	latest := version.Normalize(release.TagName)
	// 额外校验：latest 必须与 TagName 规范化后一致且无 prerelease
	if latest == "" {
		u.setUpdateResultPhase(res, updatePhaseFailed)
		res.Code = "github_error"
		res.Error = "Release tag 为空"
		return res
	}
	res.Version = latest
	if u.pinned != "" && latest != u.pinned {
		u.setUpdateResultPhase(res, updatePhaseFailed)
		res.Code = "failed"
		res.Error = fmt.Sprintf("tag 不匹配：要求 %q，实际 %q", u.pinned, latest)
		return res
	}
	if !isReleaseNewerThanCurrent(release.TagName, version.Version) {
		u.setUpdateResultPhase(res, updatePhaseUpToDate)
		res.Code = "up_to_date"
		res.Error = fmt.Sprintf("当前已是最新版本 %s", version.Version)
		return res
	}
	res.Version = latest

	u.setUpdateResultPhase(res, updatePhaseSelectAsset)
	asset, err := selectAsset(release.Assets, release.TagName, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		u.setUpdateResultPhase(res, updatePhaseFailed)
		res.Code = "failed"
		res.Error = err.Error()
		return res
	}
	assetURL := asset.BrowserDownloadURL
	if assetURL == "" {
		assetURL = asset.URL
	}
	resolvedAssetURL, err := resolveUpdateURL(assetURL, u.source, u.proxy)
	if err != nil {
		u.setUpdateResultPhase(res, updatePhaseFailed)
		res.Code = "failed"
		res.Error = err.Error()
		return res
	}
	if asset.BrowserDownloadURL != "" {
		asset.BrowserDownloadURL = resolvedAssetURL
	} else {
		asset.URL = resolvedAssetURL
	}

	tmpDir, err := os.MkdirTemp(filepath.Dir(u.exe), ".oss-update-*")
	if err != nil {
		u.setUpdateResultPhase(res, updatePhaseFailed)
		res.Code = "failed"
		res.Error = fmt.Sprintf("创建临时目录失败: %v", err)
		return res
	}
	defer os.RemoveAll(tmpDir)

	u.setUpdateResultPhase(res, updatePhaseDownload)
	prepared, err := u.downloadAsset(ctx, *asset, tmpDir)
	if err != nil {
		u.setUpdateResultPhase(res, updatePhaseFailed)
		res.Code = "failed"
		res.Error = err.Error()
		return res
	}

	u.setUpdateResultPhase(res, updatePhaseVerify)
	if u.verifier != nil {
		if err := u.verifier(prepared, latest); err != nil {
			u.setUpdateResultPhase(res, updatePhaseFailed)
			res.Code = "failed"
			res.Error = fmt.Sprintf("下载校验失败: %v", err)
			return res
		}
	}

	u.setUpdateResultPhase(res, updatePhaseBackup)
	if err := copyFile(u.exe, u.backup); err != nil {
		u.setUpdateResultPhase(res, updatePhaseFailed)
		res.Code = "failed"
		res.Error = fmt.Sprintf("备份当前二进制失败: %v", err)
		return res
	}
	res.BackupPath = u.backup

	u.setUpdateResultPhase(res, updatePhaseSwap)
	if err := swapBinary(prepared, u.exe); err != nil {
		u.setUpdateResultPhase(res, updatePhaseFailed)
		res.Code = "failed"
		res.Error = fmt.Sprintf("替换二进制失败: %v", err)
		return res
	}

	// 写入“更新待验证”标记：重启后由 StartupHealthCheck 轮询 /readyz，
	// 未就绪时据此回滚到备份二进制。标记写入失败不影响更新本身。
	if err := os.WriteFile(u.exe+".updated", []byte(latest), 0o644); err != nil {
		log.Printf("[OSS] 写入更新待验证标记失败: %v", err)
	}

	res.OK = true
	res.Code = "ok"
	u.setUpdateResultPhase(res, updatePhaseDone)
	return res
}

func (u *Updater) setUpdateResultPhase(res *UpdateResult, phase OperationState) {
	u.stateMu.Lock()
	res.Phase = phase
	res.State = phase
	cp := *res
	u.lastUpdate = &cp
	u.updatePhase = phase
	u.stateMu.Unlock()
}

func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// ExecPath 返回当前二进制路径。
func (u *Updater) ExecPath() string { return u.exe }

// BackupPath 返回备份文件路径。
func (u *Updater) BackupPath() string { return u.backup }
