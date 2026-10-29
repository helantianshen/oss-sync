// Package version 提供构建元数据与严格发布版本校验
// Version 默认为 dev，发布构建以 -ldflags 注入
// 同一份元数据供健康检查、管理版本接口与更新校验使用
package version

import "strings"

// Version 是服务端构建版本，由 ldflags 在发布构建时覆盖
// 未注入版本的源码构建标记为 dev，不能执行发布版自更新
var Version = "dev"

// Commit 是构建时的 Git commit（短哈希），由 ldflags 注入
var Commit = ""

// BuiltAt 是构建时间（RFC3339），由 ldflags 注入
var BuiltAt = ""

// Info 汇总构建元数据
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	BuiltAt string `json:"built_at,omitempty"`
}

// Get 返回当前构建信息
func Get() Info {
	return Info{
		Version: Version,
		Commit:  Commit,
		BuiltAt: BuiltAt,
	}
}

// String 返回 Version 的规范化表示（去空白、去 v 前缀后的原始值）
func String() string {
	return strings.TrimSpace(Version)
}

// IsDevelopmentVersion 判断给定版本是否为开发版本（不可用于自更新）
// 规则：空、"dev" 大小写不敏感、或不符合严格 SemVer 均视为开发版本
func IsDevelopmentVersion(v string) bool {
	s := strings.TrimSpace(v)
	if s == "" {
		return true
	}
	if strings.EqualFold(s, "dev") {
		return true
	}
	return !IsValid(s)
}

// IsDevelopment 判断当前构建是否为开发版本
func IsDevelopment() bool {
	return IsDevelopmentVersion(Version)
}
