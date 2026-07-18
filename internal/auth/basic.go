package auth

import (
	"encoding/base64"
	"strings"
)

// parseBasic 解码 base64 编码的 "user:pass"。
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
