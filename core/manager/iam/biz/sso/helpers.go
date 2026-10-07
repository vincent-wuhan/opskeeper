package sso

import (
	"crypto/rand"
	"encoding/base64"
)

// RandomToken returns a URL-safe random token of n random bytes,
// base64url-encoded. Used for the OAuth state token (CSRF defense)
// and similar short-lived secrets.
func RandomToken(n int) string {
	if n <= 0 {
		n = 32
	}
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}
