package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// Token prefixes make leaked secrets easy to identify (and to scan for).
const (
	SessionTokenPrefix = "dos_"
	APITokenPrefix     = "dop_"
)

// displayPrefixLen is how many characters of a token are stored in clear so
// people can tell their tokens apart.
const displayPrefixLen = 12

// NewToken returns a new random token secret with the given prefix, the
// SHA-256 hash to store, and a short display prefix.
func NewToken(prefix string) (secret string, hash []byte, display string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, "", err
	}
	secret = prefix + base64.RawURLEncoding.EncodeToString(b)
	return secret, HashToken(secret), secret[:displayPrefixLen], nil
}

// HashToken returns the SHA-256 hash under which a token secret is stored.
// Token secrets carry 256 bits of entropy, so a fast hash is sufficient.
func HashToken(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// LooksLikeToken reports whether s has the shape of a dispatch-ops token, to
// skip a database lookup for obviously bogus values.
func LooksLikeToken(s string) bool {
	if !strings.HasPrefix(s, SessionTokenPrefix) && !strings.HasPrefix(s, APITokenPrefix) {
		return false
	}
	return len(s) == len(APITokenPrefix)+base64.RawURLEncoding.EncodedLen(32)
}
