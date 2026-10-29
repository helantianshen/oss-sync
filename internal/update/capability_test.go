// 本文件验证开发构建与不安全入口不会进入发布版自更新
package update

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/helantianshen/oss-sync/internal/version"
)

func TestDevelopmentBuildCannotSelfUpdate(t *testing.T) {
	original := version.Version
	t.Cleanup(func() { version.Version = original })
	// 未注入版本的源码即使有可写程序，也不能冒充发布版进入换装流程
	executable := filepath.Join(t.TempDir(), "oss-server")
	if err := os.WriteFile(executable, []byte("source build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckCapability(executable, "linux", "amd64"); !errors.Is(err, ErrDevelopmentVersion) {
		t.Fatalf("unversioned build %q may self-update: %v", original, err)
	}
	for _, build := range []string{"dev", " DEV ", "", "not-a-release"} {
		t.Run(build, func(t *testing.T) {
			version.Version = build
			// 开发版本须在接触可执行文件之前拒绝，不能因路径问题掩盖错误类别
			err := CheckCapability(filepath.Join(t.TempDir(), "missing-server"), "linux", "amd64")
			if !errors.Is(err, ErrDevelopmentVersion) {
				t.Fatalf("build %q capability error = %v; want development_version", build, err)
			}
			var detail *UpdateError
			if !errors.As(err, &detail) || detail.Code != CodeDevelopmentVersion {
				t.Fatalf("development error has no machine-readable code: %v", err)
			}
		})
	}
}

func TestReleaseBuildRejectsUnsafeExecutable(t *testing.T) {
	original := version.Version
	version.Version = "2.3.4"
	t.Cleanup(func() { version.Version = original })
	dir := t.TempDir()
	executable := filepath.Join(dir, "oss-server")
	if err := os.WriteFile(executable, []byte("existing program"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckCapability(executable, "plan9", "amd64"); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("unsupported platform error = %v", err)
	}
	if err := CheckCapability(dir, "linux", "amd64"); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("directory executable error = %v", err)
	}
	link := filepath.Join(dir, "linked-server")
	if err := os.Symlink(executable, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := CheckCapability(link, "linux", "amd64"); !errors.Is(err, ErrSymlinkNotAllowed) {
		t.Fatalf("symlink executable error = %v", err)
	}
	content, err := os.ReadFile(executable)
	if err != nil || string(content) != "existing program" {
		t.Fatalf("capability rejection changed program: %q, %v", content, err)
	}
}

func TestUpdateCandidateVersionSelection(t *testing.T) {
	cases := []struct {
		name    string
		current string
		release string
		want    bool
	}{
		{"dev sees stable", "dev", "v0.3.1", true},
		{"dev excludes rc", "dev", "0.3.2-rc.1", false},
		{"invalid release", "dev", "latest", false},
		{"new stable", "0.3.1", "v0.3.2", true},
		{"same release", "0.3.1", "v0.3.1", false},
		{"no downgrade", "0.3.1", "0.2.99", false},
		{"rc to stable", "0.3.2-rc.1", "0.3.2", true},
		{"metadata not update", "0.3.1+old", "0.3.1+new", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReleaseNewerThanCurrent(tc.release, tc.current); got != tc.want {
				t.Fatalf("candidate %q for %q = %v; want %v", tc.release, tc.current, got, tc.want)
			}
		})
	}
}
