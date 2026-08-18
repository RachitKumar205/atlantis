// Package authn holds Atlantis Cloud's password handling.
//
// # Why this is its own package
//
// The console deleted its password code along with console.users, so nothing in
// this repository hashes a password any more. This is a fresh start rather than
// a move, and it is deliberately separated from the store: hashing has no
// database and one job, which makes it testable without one and hard to get
// subtly wrong in the company of unrelated code.
//
// # What a password is worth here
//
// It is one factor of two. Cloud requires a second (see the 2FA package), so a
// leaked password alone does not reach an organisation's console. That is the
// reason this package can be conservative rather than paranoid — but it is not
// a reason to be careless, because the second factor is exactly what an
// attacker who has the password will go after next.
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
// of parallelism. Memory-hard by design — the cost to an attacker with a GPU
// scales with memory rather than with arithmetic, which is the whole reason to
// prefer this over bcrypt for something written today.
//
// These are the parameters used for NEW hashes. They are not what verification
// uses: every stored hash carries the parameters it was made with, so raising
// these is safe and takes effect as people sign in. See Verify's needsRehash.
const (
	argonMemory  = 19 * 1024 // KiB
	argonTime    = 2
	argonThreads = 1
	argonSaltLen = 16
	argonKeyLen  = 32
)

// ErrMalformedHash reports a stored hash this package cannot read.
//
// Distinct from "wrong password", and the distinction is load-bearing: a
// malformed hash means the row is corrupt or was written by something else, and
// treating it as a failed sign-in would leave an account permanently unable to
// authenticate with nothing in the logs explaining why.
var ErrMalformedHash = errors.New("password hash is not in the expected format")

// Hash returns a PHC-format argon2id hash of password.
//
// The format is the standard one — $argon2id$v=19$m=..,t=..,p=..$salt$hash —
// because it carries the parameters alongside the digest. That is what makes
// the constants above adjustable later without a migration or a column: an old
// hash verifies against its own parameters, and Verify reports that it should
// be rewritten.
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
// needsRehash is only meaningful when ok is true. A caller acts on it by
// rehashing during a successful sign-in, which is the only moment the plaintext
// is available — so a parameter increase rolls forward as people use the
// product rather than needing anyone to do anything.
func Verify(password, encoded string) (ok bool, needsRehash bool, err error) {
	p, salt, want, err := parse(encoded)
	if err != nil {
		return false, false, err
	}

	got := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, uint32(len(want)))

	// Constant time, because a byte-at-a-time comparison leaks how much of a
	// guess was right — and this runs on an attacker-supplied input by
	// definition.
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

// A decoy hash for the "no such account" branch of sign-in belongs here, and is
// deliberately absent until sign-in exists.
//
// Skipping the hash for an unknown address is a real enumeration oracle —
// argon2id at these parameters takes tens of milliseconds, which is trivially
// measurable from outside — so the defence is to verify against a hash nobody
// knows and always fail. It lands with the sign-in route that calls it.
//
// Written down rather than built ahead of its call site: a decoy nothing calls
// passes any test asserting it is a well-formed hash, while defending nothing,
// and reads in review as though the branch is covered.
