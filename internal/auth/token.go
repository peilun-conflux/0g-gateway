// Package auth implements expiring HMAC tokens for object download URLs,
// the gateway-side equivalent of OBS signed URLs.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Sign returns a hex HMAC-SHA256 token binding root and expiry under secret.
func Sign(secret, root string, exp time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s|%d", root, exp.Unix())
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether token is a valid signature for root/exp and exp is
// still in the future relative to now.
func Verify(secret, root string, exp time.Time, token string, now time.Time) bool {
	if !now.Before(exp) {
		return false
	}
	return hmac.Equal([]byte(Sign(secret, root, exp)), []byte(token))
}
