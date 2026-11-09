// Package auth holds password hashing, token handling and the authenticated
// principal carried in a request context.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters, following the OWASP recommendation of 19 MiB memory,
// two iterations and one degree of parallelism as a minimum.
const (
	argonTime    = 2
	argonMemory  = 19 * 1024 // KiB
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// ErrInvalidHash means a stored password hash could not be parsed.
var ErrInvalidHash = errors.New("invalid password hash")

// hashSlots bounds how many argon2 computations run at once. Each needs
// argonMemory (19 MiB), so without a bound a burst of sign-in attempts could
// exhaust the server's memory. Twice the usable CPUs keeps them busy.
var hashSlots = make(chan struct{}, 2*runtime.GOMAXPROCS(0))

// acquireHashSlot waits for a free hashing slot, or for ctx to end.
func acquireHashSlot(ctx context.Context) (release func(), err error) {
	select {
	case hashSlots <- struct{}{}:
		return func() { <-hashSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// HashPassword returns an argon2id hash in PHC string format.
func HashPassword(password string) (string, error) {
	return HashPasswordContext(context.Background(), password)
}

// HashPasswordContext is HashPassword, giving up if ctx ends while it waits
// for a hashing slot.
func HashPasswordContext(ctx context.Context, password string) (string, error) {
	release, err := acquireHashSlot(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return hashPassword(password)
}

// hashPassword is HashPassword without waiting for a slot.
func hashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the PHC-encoded hash. The
// hash's own parameters are used, so older hashes keep working after the
// defaults change.
func VerifyPassword(password, encoded string) (bool, error) {
	return VerifyPasswordContext(context.Background(), password, encoded)
}

// VerifyPasswordContext is VerifyPassword, giving up if ctx ends while it
// waits for a hashing slot.
func VerifyPasswordContext(ctx context.Context, password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrInvalidHash
	}
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false, ErrInvalidHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrInvalidHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, ErrInvalidHash
	}
	release, err := acquireHashSlot(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	got := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is verified against when a username does not exist, so a failed
// sign-in takes about as long whether or not the user exists.
var dummyHash = sync.OnceValue(func() string {
	// Made once, without a slot: a caller computing it may already be
	// waiting for one, and all slots may be busy.
	h, err := hashPassword("dispatch-ops-dummy-password")
	if err != nil {
		panic(err)
	}
	return h
})

// BurnPasswordCheck spends the same time as a real password verification.
func BurnPasswordCheck(password string) {
	_ = BurnPasswordCheckContext(context.Background(), password)
}

// BurnPasswordCheckContext is BurnPasswordCheck, returning ctx's error if
// it ends while waiting for a hashing slot.
func BurnPasswordCheckContext(ctx context.Context, password string) error {
	_, err := VerifyPasswordContext(ctx, password, dummyHash())
	return err
}
