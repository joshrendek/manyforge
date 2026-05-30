// Package auth holds authentication primitives: password hashing, EdDSA access
// tokens, and (later) refresh-token rotation. See Constitution Principle II.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters (tunable via config in production).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// ErrPasswordMismatch is returned when a password does not match its hash.
var ErrPasswordMismatch = errors.New("password mismatch")

// HashPassword returns a PHC-formatted argon2id hash of password.
func HashPassword(password string) (string, error) {
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

// VerifyPassword constant-time compares password against a PHC argon2id hash.
func VerifyPassword(password, encoded string) error {
	salt, key, m, t, p, err := decodeHash(encoded)
	if err != nil {
		return err
	}
	computed := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(key)))
	if subtle.ConstantTimeCompare(key, computed) == 1 {
		return nil
	}
	return ErrPasswordMismatch
}

// dummyHash backs DummyVerify so a sign-in attempt for an unknown email costs the
// same as a wrong-password attempt (defeats timing-based existence oracles; FR-026).
var dummyHash, _ = HashPassword("manyforge-fixed-cost-dummy")

// DummyVerify performs a throwaway argon2id comparison to match the latency of a
// real password check. Call it on the account-not-found branch of sign-in.
func DummyVerify(password string) { _ = VerifyPassword(password, dummyHash) }

func decodeHash(encoded string) (salt, key []byte, m, t uint32, p uint8, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return nil, nil, 0, 0, 0, errors.New("invalid password hash format")
	}
	if _, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return nil, nil, 0, 0, 0, fmt.Errorf("invalid hash params: %w", err)
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return nil, nil, 0, 0, 0, err
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return nil, nil, 0, 0, 0, err
	}
	return salt, key, m, t, p, nil
}
