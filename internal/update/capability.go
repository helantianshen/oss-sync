// 更新能力检测
package update

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/oss/oss-server/internal/version"
)

// CheckCapability 校验当前进程是否具备执行自更新的前置条件。
// 依次检查：开发版本、受支持平台、可执行文件形态（常规文件、非软链）、
// 可执行文件所在目录可写。任一不满足即返回带稳定 Code 的 UpdateError。
func CheckCapability(execPath string, goos, goarch string) error {
	if version.IsDevelopmentVersion(version.Version) {
		return newUpdateError(
			CodeDevelopmentVersion,
			fmt.Sprintf("development version %q is not eligible for self-update", version.Version),
			ErrDevelopmentVersion,
		)
	}
	if !IsSupportedPlatform(goos, goarch) {
		return newUpdateError(
			CodeUnsupportedPlatform,
			fmt.Sprintf("unsupported platform %s/%s", goos, goarch),
			ErrUnsupportedPlatform,
		)
	}
	if execPath == "" {
		return newUpdateError(CodeNotRegularFile, "executable path is empty", ErrNotRegularFile)
	}
	info, err := os.Lstat(execPath)
	if err != nil {
		return newUpdateError(CodeNotRegularFile, fmt.Sprintf("cannot stat executable %q: %v", execPath, err), err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return newUpdateError(CodeSymlinkNotAllowed, fmt.Sprintf("executable %q is a symlink", execPath), ErrSymlinkNotAllowed)
	}
	if !info.Mode().IsRegular() {
		return newUpdateError(CodeNotRegularFile, fmt.Sprintf("executable %q is not a regular file", execPath), ErrNotRegularFile)
	}
	dir := filepath.Dir(execPath)
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return newUpdateError(CodeUnwritableDirectory, fmt.Sprintf("cannot stat executable directory %q: %v", dir, err), err)
	}
	if !dirInfo.IsDir() {
		return newUpdateError(CodeUnwritableDirectory, fmt.Sprintf("executable directory %q is not a directory", dir), nil)
	}
	// 目录可写性：尝试创建并立即删除临时文件，避免仅依赖权限位的误判。
	tmpFile, err := os.CreateTemp(dir, ".oss-write-check-*")
	if err != nil {
		return newUpdateError(CodeUnwritableDirectory, fmt.Sprintf("executable directory %q is not writable: %v", dir, err), err)
	}
	tmpName := tmpFile.Name()
	_ = tmpFile.Close()
	_ = os.Remove(tmpName)
	return nil
}

// CheckCurrentCapability 使用当前版本与平台校验能力。
func CheckCurrentCapability(execPath string) error {
	return CheckCapability(execPath, runtime.GOOS, runtime.GOARCH)
}
