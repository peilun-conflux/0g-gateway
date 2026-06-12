package auth

import (
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	secret := "s3cret"
	root := "0xabc"
	exp := time.Now().Add(time.Hour)
	tok := Sign(secret, root, exp)
	if tok == "" {
		t.Fatal("empty token")
	}
	if !Verify(secret, root, exp, tok, time.Now()) {
		t.Fatal("valid token rejected")
	}
}

func TestVerifyExpired(t *testing.T) {
	secret := "s"
	root := "0xabc"
	exp := time.Now().Add(-time.Minute)
	tok := Sign(secret, root, exp)
	if Verify(secret, root, exp, tok, time.Now()) {
		t.Fatal("expired token accepted")
	}
}

func TestVerifyTamper(t *testing.T) {
	secret := "s"
	exp := time.Now().Add(time.Hour)
	tok := Sign(secret, "0xaaa", exp)
	if Verify(secret, "0xbbb", exp, tok, time.Now()) {
		t.Fatal("token bound to other root accepted")
	}
	if Verify("other-secret", "0xaaa", exp, tok, time.Now()) {
		t.Fatal("wrong secret accepted")
	}
	if Verify(secret, "0xaaa", exp.Add(time.Hour), tok, time.Now()) {
		t.Fatal("token with modified expiry accepted")
	}
}
