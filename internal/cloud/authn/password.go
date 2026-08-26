// Package authn holds Atlantis Cloud's password handling.
//
// Separate from the store, because hashing needs no database and is therefore
// testable without one.
//
// A password here is one factor of two: Cloud requires a second factor, so a
// leaked password alone does not reach an organisation's console.
package authn

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters.
//
// OWASP's current recommendation: 19 MiB of memory, two iterations, one degree
// of parallelism.
//
// These apply to new hashes only. Every stored hash carries the parameters it
// was made with, so raising these is safe; see Verify's needsRehash.
const (
	argonMemory  = 19 * 1024 // KiB
	argonTime    = 2
	argonThreads = 1
	argonSaltLen = 16
	argonKeyLen  = 32
)

// ErrMalformedHash reports a stored hash this package cannot read.
//
// Distinct from a wrong password: the row is corrupt or was written by
// something else, and reporting it as a failed sign-in leaves the account
// permanently unable to authenticate with nothing in the logs.
var ErrMalformedHash = errors.New("password hash is not in the expected format")

// Hash returns a PHC-format argon2id hash of password.
//
// $argon2id$v=19$m=..,t=..,p=..$salt$hash carries the parameters alongside the
// digest, so an old hash verifies against its own and Verify reports that it
// should be rewritten. No migration or column is needed to raise them.
func Hash(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify reports whether password matches encoded, and whether the stored hash
// was made with parameters this build has since raised.
//
// needsRehash is meaningful only when ok is true. A successful sign-in is the
// only moment the plaintext is available to rehash with.
func Verify(password, encoded string) (ok bool, needsRehash bool, err error) {
	p, salt, want, err := parse(encoded)
	if err != nil {
		return false, false, err
	}

	got := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, uint32(len(want)))

	// Constant time: a byte-at-a-time comparison leaks how much of a guess was
	// right, on an input that comes from the request.
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	return true, p.memory != argonMemory || p.time != argonTime || p.threads != argonThreads, nil
}

type params struct {
	memory  uint32
	time    uint32
	threads uint8
}

func parse(encoded string) (params, []byte, []byte, error) {
	// $argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return params{}, nil, nil, ErrMalformedHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return params{}, nil, nil, ErrMalformedHash
	}
	if version != argon2.Version {
		// A different argon2 version computes a different digest from the same
		// inputs, so verifying against it would silently always fail. Said
		// rather than returned as "wrong password".
		return params{}, nil, nil, fmt.Errorf("%w: argon2 version %d, this build speaks %d",
			ErrMalformedHash, version, argon2.Version)
	}

	var p params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return params{}, nil, nil, ErrMalformedHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return params{}, nil, nil, ErrMalformedHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return params{}, nil, nil, ErrMalformedHash
	}
	if len(salt) == 0 || len(want) == 0 {
		return params{}, nil, nil, ErrMalformedHash
	}
	return p, salt, want, nil
}

// dummyHash is verified against when no account matches, so that the work a
// sign-in attempt does not reveal whether an address is registered.
//
// Computed once at package load with the current parameters. Skipping the hash
// for an unknown address is a real enumeration oracle: argon2id at these
// settings takes tens of milliseconds, which is trivially measurable from
// outside, and the response body says nothing.
var dummyHash = mustHash("atlantis: no account matches, and this exists so " +
	"that fact costs the same as one that does")

func mustHash(s string) string {
	h, err := Hash(s)
	if err != nil {
		// Only reachable if crypto/rand fails, which is not a condition this
		// process can continue through.
		panic("authn: cannot hash at startup: " + err.Error())
	}
	return h
}

// VerifyDecoy does the work of a password check against a hash nobody knows,
// and always reports failure.
//
// Called on the "no such account" branch of sign-in. The point is the time it
// takes, not the answer.
func VerifyDecoy(password string) {
	_, _, _ = Verify(password, dummyHash)
}
