// 更新服务
package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/version"
)

// Service orchestrates Manager-checked candidate download and helper handoff.
type Service struct {
	mgr *Manager
	up  *Updater
	cfg *config.Config

	onShutdown    func()
	shutdownFired atomic.Bool
}

// NewService creates a Service. mgr and up must be non-nil.
func NewService(mgr *Manager, up *Updater, cfg *config.Config) *Service {
	return &Service{mgr: mgr, up: up, cfg: cfg}
}

// Manager returns the durable manager.
func (s *Service) Manager() *Manager { return s.mgr }

// SetOnShutdown injects shutdown callback invoked only after helper launch success.
func (s *Service) SetOnShutdown(fn func()) {
	s.onShutdown = fn
	s.shutdownFired.Store(false)
}

func (s *Service) triggerShutdown() {
	if s.onShutdown == nil || !s.shutdownFired.CompareAndSwap(false, true) {
		return
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.onShutdown()
	}()
}

// StartHelperUpdate validates the checked candidate, downloads the exact asset,
// stages it and hands off to helper. Shutdown is signaled only after helper launch success.
func (s *Service) StartHelperUpdate(ctx context.Context, checkID, downloadSource, customProxy string) (*Operation, error) {
	if s.mgr == nil {
		return nil, fmt.Errorf("manager is nil")
	}
	if s.up == nil {
		return nil, fmt.Errorf("updater is nil")
	}
	if checkID == "" {
		return nil, newUpdateError(CodeCheckNotFound, "check_id is empty", ErrCheckNotFound)
	}
	cand, err := s.mgr.ValidateChecked(checkID)
	if err != nil {
		return nil, err
	}
	if err := cand.Validate(); err != nil {
		return nil, err
	}
	// Capability check before mutation.
	if err := CheckHandoffCapability(s.up.exe); err != nil {
		return nil, err
	}
	downloadSource, customProxy = s.effectiveSource(downloadSource, customProxy)
	downloadURL, err := resolveDownloadURL(cand.AssetURL, downloadSource, customProxy)
	if err != nil {
		return nil, err
	}
	// Download exact candidate asset to temp dir.
	tmpDir, err := os.MkdirTemp(filepath.Dir(s.up.exe), ".oss-download-*")
	if err != nil {
		return nil, fmt.Errorf("create download dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	asset := Asset{
		ID:                 cand.AssetID,
		Name:               cand.AssetName,
		BrowserDownloadURL: downloadURL,
		Size:               cand.Size,
		Digest:             cand.Digest,
	}
	prepared, err := s.up.downloadAsset(ctx, asset, tmpDir)
	if err != nil {
		return nil, err
	}
	readyURL := s.readyURL()
	origArgs := os.Args
	workDir, _ := os.Getwd()
	op, err := s.up.InitiateHelperHandoff(s.mgr, checkID, prepared, readyURL, origArgs, workDir)
	if err != nil {
		return nil, err
	}
	s.triggerShutdown()
	return op, nil
}

func (s *Service) readyURL() string {
	if s.cfg == nil {
		return "http://127.0.0.1:8080/readyz"
	}
	host := s.cfg.Server.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	port := s.cfg.Server.Port
	if port == 0 {
		port = 8080
	}
	return fmt.Sprintf("http://%s:%d/readyz", host, port)
}

// CheckInfo 是 WebUI 检查更新的结果，包含 durable check_id。
type CheckInfo struct {
	CheckID         string     `json:"check_id"`
	Candidate       *Candidate `json:"candidate"`
	CurrentVersion  string     `json:"current_version"`
	LatestVersion   string     `json:"latest_version"`
	UpdateAvailable bool       `json:"update_available"`
	ReleaseURL      string     `json:"release_url"`
	ExpiresAt       int64      `json:"expires_at"`
	Note            string     `json:"note,omitempty"`
}

// Check 通过配置的更新源检查 latest release，严格校验平台资产并颁发 durable check_id。
// 复用与 /api/admin/update/check 相同的校验逻辑，返回可序列化的 CheckInfo。
func (s *Service) Check(ctx context.Context) (*CheckInfo, error) {
	return s.CheckWithSource(ctx, "", "")
}

// CheckWithSource checks the latest release through the selected source.
func (s *Service) CheckWithSource(ctx context.Context, source, customProxy string) (*CheckInfo, error) {
	if s.mgr == nil {
		return nil, newUpdateError(CodeCorruptedState, "manager not initialized", ErrCorruptedState)
	}
	if s.up == nil {
		return nil, newUpdateError(CodeCorruptedState, "updater not initialized", ErrCorruptedState)
	}
	if s.up.gh == nil {
		return nil, newUpdateError(CodeCorruptedState, "github client not initialized", ErrCorruptedState)
	}
	// timeout enforced by caller; add 30s guard
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	source, customProxy = s.effectiveSource(source, customProxy)
	release, err := s.up.gh.fetchLatestFrom(cctx, source, customProxy)
	if err != nil {
		if errors.Is(err, ErrNoRelease) {
			return &CheckInfo{
				CurrentVersion:  version.Version,
				UpdateAvailable: false,
				Note:            "上游仓库暂无 Release",
			}, nil
		}
		return nil, err
	}
	asset, err := selectAsset(release.Assets, release.TagName, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	cand, err := NewCandidate(release.TagName, runtime.GOOS, runtime.GOARCH, asset.BrowserDownloadURL, release.HTMLURL, asset.Size, release.ID, asset.ID, asset.Digest)
	if err != nil {
		return nil, err
	}
	ttl := time.Hour
	if s.cfg != nil {
		ttl = s.cfg.Update.EffectiveCheckTTL()
	}
	cc, err := s.mgr.IssueChecked(*cand, ttl)
	if err != nil {
		return nil, err
	}
	updateAvailable := false
	if isReleaseNewerThanCurrent(release.TagName, version.Version) {
		updateAvailable = true
	}
	return &CheckInfo{
		CheckID:         cc.ID,
		Candidate:       cand,
		CurrentVersion:  version.Version,
		LatestVersion:   release.TagName,
		UpdateAvailable: updateAvailable,
		ReleaseURL:      release.HTMLURL,
		ExpiresAt:       cc.ExpiresAt,
	}, nil
}

func (s *Service) effectiveSource(source, customProxy string) (string, string) {
	if source != "" || s.cfg == nil {
		return source, customProxy
	}
	return s.cfg.Update.EffectiveDownloadSource(), s.cfg.Update.EffectiveDownloadProxy()
}
