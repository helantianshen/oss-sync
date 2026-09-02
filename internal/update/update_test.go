package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oss/oss-server/internal/config"
	"github.com/oss/oss-server/internal/version"
)

func TestSelectAsset(t *testing.T) {
	assets := []Asset{
		{ID: 1, Name: "oss-server_1.0.0_darwin_arm64.tar.gz", Size: 100, Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BrowserDownloadURL: "https://example.com/a.tar.gz"},
		{ID: 2, Name: "oss-server_1.0.0_linux_amd64.tar.gz", Size: 100, Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", BrowserDownloadURL: "https://example.com/b.tar.gz"},
		{ID: 3, Name: "checksums.txt", Size: 100, Digest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", BrowserDownloadURL: "https://example.com/c.txt"},
	}
	got, err := selectAsset(assets, "v1.0.0", "linux", "amd64")
	if err != nil {
		t.Fatalf("selectAsset: %v", err)
	}
	if got.Name != assets[1].Name {
		t.Errorf("selected %q, want %q", got.Name, assets[1].Name)
	}

	if _, err := selectAsset(assets, "v1.0.0", "freebsd", "arm64"); err == nil {
		t.Error("expected error for unsupported platform with multiple assets")
	}

	// exact match required: even single asset must match expected name
	single := []Asset{{Name: "oss-server.bin"}}
	if _, err := selectAsset(single, "v1.0.0", "linux", "amd64"); err == nil {
		t.Error("expected error for asset name mismatch, single-asset fallback must not be accepted")
	}
	// empty assets
	if _, err := selectAsset(nil, "v1.0.0", "linux", "amd64"); err == nil {
		t.Error("expected error for empty assets")
	}
	// asset mismatch: name contains platform substring but not exact
	legacy := []Asset{{Name: "oss-server-linux-amd64-v1.0.0.tar.gz"}}
	if _, err := selectAsset(legacy, "v1.0.0", "linux", "amd64"); err == nil {
		t.Error("legacy permissive name should not be accepted; exact AssetName required")
	}
	// version mismatch
	if _, err := selectAsset(assets, "v2.0.0", "linux", "amd64"); err == nil {
		t.Error("expected error for version mismatch")
	}
}

func TestCheckExecutableMagic(t *testing.T) {
	tmp := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, b, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	elf := []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}
	pe := []byte{'M', 'Z', 0x90, 0x00}
	macho := []byte{0xcf, 0xfa, 0xed, 0xfe, 7, 0, 0, 1}
	text := []byte("#!/bin/sh\necho hi\n")

	if err := checkExecutableMagic(write("a.bin", elf), "linux"); err != nil {
		t.Errorf("linux ELF: %v", err)
	}
	if err := checkExecutableMagic(write("b.bin", pe), "linux"); err == nil {
		t.Error("windows PE should fail linux check")
	}
	if err := checkExecutableMagic(write("c.bin", pe), "windows"); err != nil {
		t.Errorf("windows PE: %v", err)
	}
	if err := checkExecutableMagic(write("d.bin", elf), "windows"); err == nil {
		t.Error("ELF should fail windows check")
	}
	if err := checkExecutableMagic(write("e.bin", macho), "darwin"); err != nil {
		t.Errorf("darwin Mach-O: %v", err)
	}
	if err := checkExecutableMagic(write("f.bin", text), "linux"); err == nil {
		t.Error("script should fail linux check")
	}
	if err := checkExecutableMagic(write("g.bin", elf), "freebsd"); err != nil {
		t.Errorf("unknown GOOS should be skipped, got %v", err)
	}
	if err := checkExecutableMagic(write("h.bin", []byte{'x'}), "linux"); err == nil {
		t.Error("tiny file should fail")
	}
}

func TestSwapBinary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "server")
	prepared := filepath.Join(dir, "server.new")
	if err := os.WriteFile(target, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prepared, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := swapBinary(prepared, target); err != nil {
		t.Fatalf("swapBinary: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "new-binary" {
		t.Errorf("target content = %q, want new-binary", got)
	}
	if _, err := os.Stat(prepared); !os.IsNotExist(err) {
		t.Errorf("prepared file should be gone after swap")
	}
}

func TestDownloadFile(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 1024)
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.Write(content)
	})
	mux.HandleFunc("/mismatch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.Write(content)
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	client := srv.Client()

	dir := t.TempDir()
	dest := filepath.Join(dir, "bin")
	if err := downloadFile(context.Background(), client, srv.URL+"/ok", dest, int64(len(content)), "", "", ""); err != nil {
		t.Fatalf("happy download: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, content) {
		t.Error("downloaded content mismatch")
	}

	if err := downloadFile(context.Background(), client, srv.URL+"/mismatch", dest, int64(len(content))+5, "", "", ""); err == nil {
		t.Error("expected size mismatch error")
	}
	if err := downloadFile(context.Background(), client, srv.URL+"/missing", dest, 0, "", "", ""); err == nil {
		t.Error("expected non-200 error")
	}
}

func fakeExecBytes() []byte {
	switch runtime.GOOS {
	case "windows":
		return []byte{'M', 'Z', 0x90, 0x00, 'f', 'a', 'k', 'e'}
	case "linux":
		return []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 'f', 'a', 'k', 'e'}
	case "darwin":
		return []byte{0xcf, 0xfa, 0xed, 0xfe, 7, 0, 0, 1, 'f', 'a', 'k', 'e'}
	default:
		return []byte("#!/bin/sh\n")
	}
}

func makeTarGz(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeZip(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractBinaryFromArchive(t *testing.T) {
	exe := fakeExecBytes()
	readme := []byte("hello")

	tarGz := makeTarGz(t, map[string][]byte{
		"bin/oss-server": exe,
		"README.md":      readme,
	})
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "server.tar.gz")
	if err := os.WriteFile(tarPath, tarGz, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := extractBinary(tarPath, dir)
	if err != nil {
		t.Fatalf("extract tar.gz: %v", err)
	}
	data, _ := os.ReadFile(got)
	if !bytes.Equal(data, exe) {
		t.Errorf("extracted tar.gz binary mismatch: %q", data)
	}

	zipBytes := makeZip(t, map[string][]byte{
		"oss-server": exe,
		"notes.txt":  readme,
	})
	zipPath := filepath.Join(dir, "server.zip")
	if err := os.WriteFile(zipPath, zipBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = extractBinary(zipPath, dir)
	if err != nil {
		t.Fatalf("extract zip: %v", err)
	}
	data, _ = os.ReadFile(got)
	if !bytes.Equal(data, exe) {
		t.Errorf("extracted zip binary mismatch: %q", data)
	}

	// 没有可执行文件时应报错。
	badTar := makeTarGz(t, map[string][]byte{"README.md": readme})
	badPath := filepath.Join(dir, "bad.tar.gz")
	if err := os.WriteFile(badPath, badTar, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBinary(badPath, dir); err == nil {
		t.Error("expected error extracting archive without executable")
	}
}

// mockUpstream 模拟 GitHub Release API 与二进制下载。
type mockUpstream struct {
	srv *httptest.Server
}

func expectedTestAssetName(tag string) string {
	n, err := AssetName(tag, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		panic(err)
	}
	return n
}

func newMockUpstream(t *testing.T, tag string, content []byte) *mockUpstream {
	return newMockUpstreamAsset(t, tag, expectedTestAssetName(tag), content)
}

func wrapContentIfArchive(t *testing.T, assetName string, content []byte) []byte {
	t.Helper()
	l := strings.ToLower(assetName)
	if strings.HasSuffix(l, ".tar.gz") || strings.HasSuffix(l, ".tgz") {
		return makeTarGz(t, map[string][]byte{"oss-server": content})
	}
	if strings.HasSuffix(l, ".zip") {
		return makeZip(t, map[string][]byte{"oss-server": content})
	}
	return content
}

func digestOfBytes(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

func newMockUpstreamAssetNoWrap(t *testing.T, tag, assetName string, content []byte) *mockUpstream {
	t.Helper()
	var srv *httptest.Server
	downloadPath := "/downloads/" + assetName
	digest := digestOfBytes(content)
	mux := http.NewServeMux()
	mux.HandleFunc(downloadPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.Write(content)
	})
	mux.HandleFunc("/repos/fake/oss-sync/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w,
			`{"id":1001,"tag_name":%q,"html_url":%q,"draft":false,"prerelease":false,"assets":[{"id":2001,"name":%q,"browser_download_url":%q,"size":%d,"digest":%q}]}`,
			tag, "https://example.com/releases/tag/"+tag, assetName, srv.URL+downloadPath, len(content), digest)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &mockUpstream{srv: srv}
}

func newMockUpstreamAsset(t *testing.T, tag, assetName string, content []byte) *mockUpstream {
	t.Helper()
	serveContent := wrapContentIfArchive(t, assetName, content)
	digest := digestOfBytes(serveContent)
	var srv *httptest.Server
	downloadPath := "/downloads/" + assetName
	mux := http.NewServeMux()
	mux.HandleFunc(downloadPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(serveContent)))
		w.Write(serveContent)
	})
	mux.HandleFunc("/repos/fake/oss-sync/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w,
			`{"id":1001,"tag_name":%q,"html_url":%q,"draft":false,"prerelease":false,"assets":[{"id":2001,"name":%q,"browser_download_url":%q,"size":%d,"digest":%q}]}`,
			tag, "https://example.com/releases/tag/"+tag, assetName, srv.URL+downloadPath, len(serveContent), digest)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &mockUpstream{srv: srv}
}

func newMockNoRelease(t *testing.T) *mockUpstream {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/fake/oss-sync/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &mockUpstream{srv: srv}
}

func testCfg() *config.Config {
	return &config.Config{
		Update: config.UpdateConfig{GitHubRepo: "fake/oss-sync"},
	}
}

func newTestUpdater(t *testing.T, up *mockUpstream, exePath string) *Updater {
	t.Helper()
	u, err := NewUpdater(testCfg(), Options{
		ExecPath:   exePath,
		APIBase:    up.srv.URL,
		HTTPClient: up.srv.Client(),
		Verifier:   func(path, wantVersion string) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewUpdater: %v", err)
	}
	return u
}

func TestUpdate_HappyPath(t *testing.T) {
	content := fakeExecBytes()
	up := newMockUpstream(t, "v9.9.9", content)
	exePath := filepath.Join(t.TempDir(), "oss-server")
	if err := os.WriteFile(exePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := newTestUpdater(t, up, exePath)

	res := u.Update(context.Background())
	if !res.OK || res.Code != "ok" {
		t.Fatalf("Update: %+v", res)
	}
	if res.Version != "9.9.9" {
		t.Errorf("version = %q, want 9.9.9", res.Version)
	}
	got, _ := os.ReadFile(exePath)
	if !bytes.Equal(got, content) {
		t.Errorf("exe content = %q, want new binary", got)
	}
	backup, _ := os.ReadFile(res.BackupPath)
	if string(backup) != "old-binary" {
		t.Errorf("backup content = %q, want old-binary", backup)
	}

	// 更新成功后应写入“待验证”标记，供重启后的自检闭环消费。
	if _, err := os.Stat(exePath + ".updated"); err != nil {
		t.Errorf("更新后应写入待验证标记: %v", err)
	}

	st := u.Status()
	if st.UpdateInProgress {
		t.Errorf("update should not be in progress after completion: %+v", st)
	}
	if st.LastUpdate == nil || !st.LastUpdate.OK {
		t.Errorf("unexpected status after update: %+v", st)
	}
}

func TestUpdate_FromArchive(t *testing.T) {
	exe := fakeExecBytes()
	assetName := expectedTestAssetName("v8.8.8")
	var archive []byte
	if strings.HasSuffix(strings.ToLower(assetName), ".zip") {
		archive = makeZip(t, map[string][]byte{
			"dist/oss-server": exe,
			"README.md":       []byte("readme"),
		})
	} else {
		archive = makeTarGz(t, map[string][]byte{
			"dist/oss-server": exe,
			"README.md":       []byte("readme"),
		})
	}
	up := newMockUpstreamAssetNoWrap(t, "v8.8.8", assetName, archive)
	exePath := filepath.Join(t.TempDir(), "oss-server")
	if err := os.WriteFile(exePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := newTestUpdater(t, up, exePath)

	res := u.Update(context.Background())
	if !res.OK || res.Code != "ok" {
		t.Fatalf("Update from archive: %+v", res)
	}
	got, _ := os.ReadFile(exePath)
	if !bytes.Equal(got, exe) {
		t.Errorf("exe after archive update = %q, want extracted binary", got)
	}
}

func TestUpdate_AlreadyLatest(t *testing.T) {
	orig := version.Version
	version.Version = "0.1.0"
	t.Cleanup(func() { version.Version = orig })

	up := newMockUpstream(t, "v0.0.1", fakeExecBytes())
	exePath := filepath.Join(t.TempDir(), "oss-server")
	if err := os.WriteFile(exePath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := newTestUpdater(t, up, exePath)
	res := u.Update(context.Background())
	if res.OK || res.Code != "up_to_date" {
		t.Errorf("Update already-latest: %+v", res)
	}
}

func TestUpdate_NoRelease(t *testing.T) {
	up := newMockNoRelease(t)
	exePath := filepath.Join(t.TempDir(), "oss-server")
	if err := os.WriteFile(exePath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := newTestUpdater(t, up, exePath)
	res := u.Update(context.Background())
	if res.OK || res.Code != "github_error" {
		t.Errorf("Update no-release: %+v", res)
	}
	if got, _ := os.ReadFile(exePath); string(got) != "old" {
		t.Error("binary should be untouched on failure")
	}
	if _, err := os.Stat(u.backup); !os.IsNotExist(err) {
		t.Error("no backup should be created on failure")
	}
}

func TestUpdate_InProgress(t *testing.T) {
	up := newMockUpstream(t, "v9.9.9", fakeExecBytes())
	exePath := filepath.Join(t.TempDir(), "oss-server")
	if err := os.WriteFile(exePath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := newTestUpdater(t, up, exePath)
	u.running.Store(true)
	res := u.Update(context.Background())
	if res.OK || res.Code != "in_progress" {
		t.Errorf("Update in-progress: %+v", res)
	}
	u.running.Store(false)
}

func TestUpdate_VerifierRejectsBadBinary(t *testing.T) {
	up := newMockUpstream(t, "v9.9.9", []byte("not-an-executable"))
	exePath := filepath.Join(t.TempDir(), "oss-server")
	if err := os.WriteFile(exePath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	u, err := NewUpdater(testCfg(), Options{
		ExecPath:   exePath,
		APIBase:    up.srv.URL,
		HTTPClient: up.srv.Client(),
		Verifier: func(path, want string) error {
			return fmt.Errorf("校验失败: %s 不是可执行文件", path)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := u.Update(context.Background())
	if res.OK || res.Code != "failed" {
		t.Errorf("Update verifier reject: %+v", res)
	}
	if got, _ := os.ReadFile(exePath); string(got) != "old" {
		t.Error("binary should be untouched when verification fails")
	}
}

func TestCheckUpdate_Results(t *testing.T) {
	up := newMockUpstream(t, "v9.9.9", fakeExecBytes())
	exePath := filepath.Join(t.TempDir(), "oss-server")
	u := newTestUpdater(t, up, exePath)

	res, err := u.CheckUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if !res.UpdateAvailable || res.LatestVersion != "v9.9.9" {
		t.Errorf("CheckUpdate result: %+v", res)
	}

	// 状态应记录最近一次检查。
	st := u.Status()
	if st.LastCheck == nil || !st.LastCheck.UpdateAvailable {
		t.Errorf("status last check: %+v", st.LastCheck)
	}
}

func TestCheckUpdate_NoReleaseIsBenign(t *testing.T) {
	up := newMockNoRelease(t)
	exePath := filepath.Join(t.TempDir(), "oss-server")
	u := newTestUpdater(t, up, exePath)

	res, err := u.CheckUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckUpdate no-release should not error, got %v", err)
	}
	if res.UpdateAvailable || res.Note == "" {
		t.Errorf("CheckUpdate no-release result: %+v", res)
	}
}

func TestCheckUpdate_RateLimited(t *testing.T) {
	up := newMockUpstream(t, "v9.9.9", fakeExecBytes())
	exePath := filepath.Join(t.TempDir(), "oss-server")
	cfg := testCfg()
	cfg.Update.CheckLimit = 1
	u, err := NewUpdater(cfg, Options{
		ExecPath:   exePath,
		APIBase:    up.srv.URL,
		HTTPClient: up.srv.Client(),
		Verifier:   func(string, string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.CheckUpdate(context.Background()); err != nil {
		t.Fatalf("first check: %v", err)
	}
	if _, err := u.CheckUpdate(context.Background()); err == nil {
		t.Fatal("expected rate-limit error on second check")
	}
}

func TestServiceCheck_usesConfiguredSourceForReleaseAPI(t *testing.T) {
	var requestPath string
	content := fakeExecBytes()
	assetName := expectedTestAssetName("v9.9.9")
	digest := digestOfBytes(content)
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.EscapedPath()
		if !strings.Contains(requestPath, "/mirror/https://") {
			http.Error(w, "missing source prefix", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w,
			`{"id":1001,"tag_name":"v9.9.9","html_url":%q,"draft":false,"prerelease":false,"assets":[{"id":2001,"name":%q,"browser_download_url":%q,"size":%d,"digest":%q}]}`,
			srv.URL+"/releases/v9.9.9", assetName, srv.URL+"/assets/"+assetName, len(content), digest)
	}))
	t.Cleanup(srv.Close)

	dataDir := t.TempDir()
	exePath := filepath.Join(t.TempDir(), "oss-server")
	if err := os.WriteFile(exePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Storage: config.StorageConfig{DataDir: dataDir},
		Update: config.UpdateConfig{
			GitHubRepo:     "fake/oss-sync",
			DownloadSource: "custom",
			DownloadProxy:  srv.URL + "/mirror",
		},
	}
	mgr, err := NewManager(dataDir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	up, err := NewUpdater(cfg, Options{
		ExecPath:   exePath,
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
		Verifier:   func(string, string) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewUpdater: %v", err)
	}
	svc := NewService(mgr, up, cfg)
	info, err := svc.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.CheckID == "" || !info.UpdateAvailable {
		t.Fatalf("unexpected check result: %+v", info)
	}
	if requestPath == "" {
		t.Fatal("release API was not requested")
	}
}

func TestTriggerRestart_CallsCallbackOnce(t *testing.T) {
	up := newMockUpstream(t, "v9.9.9", fakeExecBytes())
	exePath := filepath.Join(t.TempDir(), "oss-server")
	u := newTestUpdater(t, up, exePath)

	calls := make(chan struct{}, 2)
	u.SetOnUpdated(func() { calls <- struct{}{} })
	u.TriggerRestart()
	u.TriggerRestart()
	<-calls // 第一次回调
	select {
	case <-calls:
		t.Error("callback should fire only once")
	case <-time.After(500 * time.Millisecond):
	}
}
