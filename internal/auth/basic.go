package auth

import (
	"encoding/base64"
	"strings"
)

// parseBasic 解码 base64 编码的 "user:pass"。
// 用标准库 net/http 的 r.BasicAuth() 也能做，但直接拿 header 字符串处理更省事。
func parseBasic(payload string) (string, string, bool) {
	dec, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", "", false
	}
	idx := strings.IndexByte(string(dec), ':')
	if idx < 0 {
		return "", "", false
	}
	return string(dec[:idx]), string(dec[idx+1:]), true
}
