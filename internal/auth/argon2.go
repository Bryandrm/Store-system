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

// Argon2id parameters, tuned for a 1 GB box.
//
// 19 MiB is OWASP's second recommended profile. The more common 64 MiB
// recommendation is a self-inflicted OOM here: two concurrent logins would
// allocate 128 MiB on a machine that only has ~950 MB usable and is already
// running Postgres. It is paired with a strict rate limit on the login route,
// without which this function is a one-line memory DoS.
const (
	argonMemoryKiB = 19 * 1024 // 19 MiB
	argonTime      = 2
	argonThreads   = 1
	argonSaltBytes = 16
	argonKeyBytes  = 32
)

var (
	// ErrInvalidHashFormat means the stored string is not a hash this code
	// wrote. It is a data problem, never a wrong password.
	ErrInvalidHashFormat = errors.New("auth: password hash has an unrecognized format")

	// ErrIncompatibleVersion guards against a future argon2 version silently
	// verifying against different rules.
	ErrIncompatibleVersion = errors.New("auth: incompatible argon2 version")
)

// HashPassword returns a PHC-format encoded hash, which carries its own
// parameters. Storing them alongside the digest is what makes it possible to
// raise the cost later without invalidating existing passwords.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: could not read random salt: %w", err)
	}

	digest := argon2.IDKey([]byte(password), salt,
		argonTime, argonMemoryKiB, argonThreads, argonKeyBytes)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	), nil
}

// VerifyPassword reports whether password matches the encoded hash.
//
// The comparison is constant-time: a byte-by-byte comparison would leak, via
// timing, how much of the digest matched.
func VerifyPassword(password, encoded string) (bool, error) {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), salt,
		params.time, params.memoryKiB, params.threads, uint32(len(want)))

	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

type argonParams struct {
	memoryKiB uint32
	time      uint32
	threads   uint8
}

func decodeHash(encoded string) (params argonParams, salt, digest []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return params, nil, nil, ErrInvalidHashFormat
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return params, nil, nil, ErrInvalidHashFormat
	}
	if version != argon2.Version {
		return params, nil, nil, ErrIncompatibleVersion
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&params.memoryKiB, &params.time, &params.threads); err != nil {
		return params, nil, nil, ErrInvalidHashFormat
	}

	if salt, err = base64.RawStdEncoding.Strict().DecodeString(parts[4]); err != nil {
		return params, nil, nil, ErrInvalidHashFormat
	}
	if digest, err = base64.RawStdEncoding.Strict().DecodeString(parts[5]); err != nil {
		return params, nil, nil, ErrInvalidHashFormat
	}

	return params, salt, digest, nil
}
