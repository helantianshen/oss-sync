package jwt

import (
	"testing"
	"time"
)

func TestSignAndParse_RoundTrip(t *testing.T) {
	secret := "test-secret"
	tok, err := Sign(secret, Claims{UserID: 42, Username: "alice", Role: "admin"}, time.Hour)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
	c, err := Parse(secret, tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.UserID != 42 || c.Username != "alice" || c.Role != "admin" {
		t.Errorf("claims mismatch: %+v", c)
	}
}

func TestParse_Expired(t *testing.T) {
	secret := "s"
	tok, _ := Sign(secret, Claims{UserID: 1}, -time.Minute)
	if _, err := Parse(secret, tok); err != ErrExpired {
		t.Errorf("expected ErrExpired, got %v", err)
	}
}

func TestParse_WrongSecret(t *testing.T) {
	tok, _ := Sign("secret-a", Claims{UserID: 1}, time.Hour)
	if _, err := Parse("secret-b", tok); err != ErrInvalidToken {
		t.Errorf("expected ErrInvalidToken, got %v", err)
	}
}

func TestParse_Malformed(t *testing.T) {
	cases := []string{"", "abc", "a.b", "a.b.c.d"}
	for _, tc := range cases {
		if _, err := Parse("s", tc); err == nil {
			t.Errorf("expected error for %q", tc)
		}
	}
}

func TestParse_EmptySecret(t *testing.T) {
	if _, err := Sign("", Claims{}, time.Hour); err == nil {
		t.Error("expected error signing with empty secret")
	}
	if _, err := Parse("", "a.b.c"); err == nil {
		t.Error("expected error parsing with empty secret")
	}
}

func TestMustSign_PanicsOnError(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	MustSign("", Claims{}, time.Hour)
}
