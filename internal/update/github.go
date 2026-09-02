// GitHub 更新源
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/oss/oss-server/internal/auth"
	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/version"
)

// maxReleaseBody 限制 Release 元数据响应体大小。
const maxReleaseBody = 4 << 20 // 4 MiB

var (
	// ErrNoRelease 表示上游仓库没有任何 Release。
	ErrNoRelease = errors.New("上游仓库暂无 Release")
	// errRateLimited 表示请求过于频繁，被限流拒绝。
	errRateLimited = errors.New("请求过于频繁，请稍后再试")
)

// Release 是 GitHub Release 元数据的子集，包含不可变身份与严格校验所需字段。
type Release struct {
	ID          int64   `json:"id"`
	TagName     string  `json:"tag_name"`
	Name        string  `json:"name"`
	Draft       bool    `json:"draft"`
	Prerelease  bool    `json:"prerelease"`
	PublishedAt string  `json:"published_at"`
	HTMLURL     string  `json:"html_url"`
	Assets      []Asset `json:"assets"`
}

// Asset 是 Release 中的一个可下载资产，包含不可变身份与校验所需字段。
type Asset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	URL                string `json:"url"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"`
}

// GitHubClient 封装对 GitHub API 的请求：超时、可选 Token、限流。
type GitHubClient struct {
	http    *http.Client
	apiBase string // 测试中可指向本地 mock 服务
	owner   string
	repo    string
	token   string
	limiter *auth.AttemptLimiter
}

func (c *GitHubClient) fetchLatestFrom(ctx context.Context, source, customProxy string) (*Release, error) {
	client, err := c.withSource(source, customProxy)
	if err != nil {
		return nil, err
	}
	return client.fetchLatest(ctx)
}

func (c *GitHubClient) withSource(source, customProxy string) (*GitHubClient, error) {
	apiBase, err := resolveUpdateURL(c.apiBase, source, customProxy)
	if err != nil {
		return nil, err
	}
	client := *c
	client.apiBase = apiBase
	client.http = clientWithSafeRedirect(c.http, apiBase)
	if source != "" && strings.TrimSpace(source) != "official" {
		client.token = ""
	}
	return &client, nil
}

func newGitHubClient(cfg *config.Config, httpClient *http.Client) *GitHubClient {
	owner, repo := "helantianshen", "oss-sync"
	if full := cfg.Update.EffectiveGitHubRepo(); full != "" {
		if o, r, ok := strings.Cut(full, "/"); ok && o != "" && r != "" {
			owner, repo = o, r
		}
	}
	client := httpClient
	if client == nil {
		client = &http.Client{Timeout: cfg.Update.EffectiveTimeout()}
	}
	return &GitHubClient{
		http:    client,
		apiBase: "https://api.github.com",
		owner:   owner,
		repo:    repo,
		token:   os.Getenv("OSS_GITHUB_TOKEN"),
		limiter: auth.NewAttemptLimiter(cfg.Update.EffectiveCheckLimit(), cfg.Update.EffectiveCheckWindow()),
	}
}

// fetchLatest 拉取最新的正式 Release，强制执行严格稳定版本契约。
func (c *GitHubClient) fetchLatest(ctx context.Context) (*Release, error) {
	if !c.limiter.Allow("github-api") {
		return nil, errRateLimited
	}
	u := fmt.Sprintf("%s/repos/%s/%s/releases/latest", c.apiBase, c.owner, c.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "oss-sync-updater/"+version.Version)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询 GitHub latest release 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API 返回 %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReleaseBody))
	if err != nil {
		return nil, fmt.Errorf("读取 GitHub 响应失败: %w", err)
	}
	var rel Release
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("解析 GitHub 响应失败: %w", err)
	}
	if err := validateRelease(&rel); err != nil {
		// 按契约，malformed / prerelease / draft 均视为无可用 Release
		// 若为稳定版本校验失败，返回可被调用方识别为 github_error 的包装错误
		// 但保持 ErrNoRelease 语义以免将草稿误判为可用更新
		if errors.Is(err, ErrInvalidVersion) {
			return nil, fmt.Errorf("%w: %v", ErrNoRelease, err)
		}
		return nil, err
	}
	return &rel, nil
}

// validateRelease 对 Release 执行严格校验：不可变 ID、稳定 tag、draft/prerelease 拒绝。
func validateRelease(rel *Release) error {
	if rel == nil {
		return ErrNoRelease
	}
	if rel.ID == 0 {
		return newUpdateError(CodeInvalidAsset, "release id is missing or zero", ErrInvalidAsset)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return ErrNoRelease
	}
	if rel.Draft || rel.Prerelease {
		return ErrNoRelease
	}
	sv, err := version.Parse(rel.TagName)
	if err != nil {
		return newUpdateError(CodeInvalidVersion, fmt.Sprintf("invalid release tag %q", rel.TagName), err)
	}
	if sv.Prerelease != "" {
		return ErrNoRelease
	}
	if strings.TrimSpace(rel.HTMLURL) != "" && !isHTTPSURL(rel.HTMLURL) && !isLoopbackURL(rel.HTMLURL) {
		return newUpdateError(CodeInvalidURL, fmt.Sprintf("release html_url must be https, got %q", rel.HTMLURL), ErrInvalidURL)
	}
	return nil
}

// selectAsset 为当前平台挑选可下载资产：仅接受与 AssetName 精确匹配的资产，且要求唯一、含有效 sha256 digest。
func selectAsset(assets []Asset, tag, goos, goarch string) (*Asset, error) {
	if len(assets) == 0 {
		return nil, errors.New("该 Release 没有可下载的资产")
	}
	expected, err := AssetName(tag, goos, goarch)
	if err != nil {
		return nil, err
	}
	// Normalize tag for strict version check: tag must be stable.
	sv, err := version.Parse(tag)
	if err != nil {
		return nil, newUpdateError(CodeInvalidVersion, fmt.Sprintf("invalid tag %q", tag), err)
	}
	if sv.Prerelease != "" {
		return nil, newUpdateError(CodeInvalidVersion, fmt.Sprintf("prerelease tag %q not allowed", tag), ErrInvalidVersion)
	}
	var matched []*Asset
	for i := range assets {
		if assets[i].Name == expected {
			matched = append(matched, &assets[i])
		}
	}
	if len(matched) == 0 {
		names := make([]string, 0, len(assets))
		for _, a := range assets {
			names = append(names, a.Name)
		}
		return nil, fmt.Errorf("没有匹配 %s/%s 的资产 %q，可用资产: %s", goos, goarch, expected, strings.Join(names, ", "))
	}
	if len(matched) > 1 {
		return nil, newUpdateError(CodeInvalidAsset, fmt.Sprintf("duplicate asset %q count %d", expected, len(matched)), ErrInvalidAsset)
	}
	a := matched[0]
	if a.ID == 0 {
		return nil, newUpdateError(CodeInvalidAsset, fmt.Sprintf("asset %q id is missing or zero", expected), ErrInvalidAsset)
	}
	if a.Size <= 0 {
		return nil, newUpdateError(CodeInvalidSize, fmt.Sprintf("asset %q size must be positive, got %d", expected, a.Size), ErrInvalidSize)
	}
	if a.Size > maxDownloadSize {
		return nil, newUpdateError(CodeInvalidSize, fmt.Sprintf("asset %q size %d exceeds limit %d", expected, a.Size, maxDownloadSize), ErrInvalidSize)
	}
	if !isValidDigest(a.Digest) {
		return nil, newUpdateError(CodeInvalidAsset, fmt.Sprintf("asset %q digest %q is missing or malformed, want sha256:<64 hex>", expected, a.Digest), ErrInvalidAsset)
	}
	if a.BrowserDownloadURL == "" && a.URL == "" {
		return nil, newUpdateError(CodeInvalidURL, fmt.Sprintf("asset %q has no download url", expected), ErrInvalidURL)
	}
	if a.BrowserDownloadURL != "" && !isValidAssetURL(a.BrowserDownloadURL) {
		return nil, newUpdateError(CodeInvalidURL, fmt.Sprintf("asset %q browser_download_url must be https, got %q", expected, a.BrowserDownloadURL), ErrInvalidURL)
	}
	if a.URL != "" && !isValidAssetURL(a.URL) {
		return nil, newUpdateError(CodeInvalidURL, fmt.Sprintf("asset %q url must be https, got %q", expected, a.URL), ErrInvalidURL)
	}
	// 生产环境要求 https；测试环境允许 http loopback，最终下载层会二次校验并拒绝 downgrade
	if a.BrowserDownloadURL != "" && !isHTTPSURL(a.BrowserDownloadURL) && !isLoopbackURL(a.BrowserDownloadURL) {
		return nil, newUpdateError(CodeInvalidURL, fmt.Sprintf("asset %q browser_download_url must be https, got %q", expected, a.BrowserDownloadURL), ErrInvalidURL)
	}
	if a.URL != "" && !isHTTPSURL(a.URL) && !isLoopbackURL(a.URL) {
		return nil, newUpdateError(CodeInvalidURL, fmt.Sprintf("asset %q url must be https, got %q", expected, a.URL), ErrInvalidURL)
	}
	return a, nil
}

// isHTTPSURL 校验字符串为 https URL。
func isHTTPSURL(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && u.Host != ""
}

func isValidAssetURL(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

func isLoopbackURL(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// isValidDigest 校验 sha256:<64 hex> 格式。
func isValidDigest(d string) bool {
	if !strings.HasPrefix(d, "sha256:") {
		return false
	}
	hexPart := strings.TrimPrefix(d, "sha256:")
	if len(hexPart) != 64 {
		return false
	}
	for _, c := range hexPart {
		if !isHexChar(c) {
			return false
		}
	}
	return true
}

func isHexChar(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
