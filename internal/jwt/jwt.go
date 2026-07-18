// Package jwt 提供最小化 HS256 JWT 实现。
//
// 仅支持签发/解析 HS256 + claim 标准 iat/exp/sub。
// 不引入第三方库（golang-jwt 等），保持依赖精简。
package jwt

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalidToken = errors.New("invalid jwt token")
	ErrExpired     = errors.New("jwt token expired")
)

// Claims 是 JWT 的负载。Phase 3 仅用 sub=user_id、username、role。
type Claims struct {
	UserID   uint   `json:"uid"`
	Username string `json:"username"`
	Role     string `json:"role"`
	IssuedAt int64  `json:"iat"`
	ExpAt    int64  `json:"exp"`
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// Sign 用 HS256 签发一个 token，ttl 控制过期时间。
func Sign(secret string, claims Claims, ttl time.Duration) (string, error) {
	if secret == "" {
		return "", errors.New("jwt secret is empty")
	}
	now := time.Now()
	if claims.IssuedAt == 0 {
		claims.IssuedAt = now.Unix()
	}
	claims.ExpAt = now.Add(ttl).Unix()

	h := header{Alg: "HS256", Typ: "JWT"}
	hBytes, _ := json.Marshal(h)
	payloadBytes, _ := json.Marshal(claims)

	seg1 := base64.RawURLEncoding.EncodeToString(hBytes)
	seg2 := base64.RawURLEncoding.EncodeToString(payloadBytes)
	signingInput := seg1 + "." + seg2
	sig := hmacSha256(secret, signingInput)
	return signingInput + "." + sig, nil
}

// Parse 解析并校验签名、过期时间。
func Parse(secret, token string) (*Claims, error) {
	if secret == "" {
		return nil, errors.New("jwt secret is empty")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrInvalidToken
	}
	signingInput := parts[0] + "." + parts[1]
	expectedSig := hmacSha256(secret, signingInput)
	if !hmac.Equal([]byte(parts[2]), []byte(expectedSig)) {
		return nil, ErrInvalidToken
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrInvalidToken
	}
	var c Claims
	if err := json.Unmarshal(payloadBytes, &c); err != nil {
		return nil, ErrInvalidToken
	}
	if c.ExpAt > 0 && time.Now().Unix() > c.ExpAt {
		return nil, ErrExpired
	}
	return &c, nil
}

func hmacSha256(secret, input string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(input))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// MustSign 仅供测试用：出错直接 panic。
func MustSign(secret string, claims Claims, ttl time.Duration) string {
	t, err := Sign(secret, claims, ttl)
	if err != nil {
		panic(fmt.Sprintf("jwt.Sign failed: %v", err))
	}
	return t
}
