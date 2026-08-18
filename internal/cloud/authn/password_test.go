package authn

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func itoa(n int) string   { return strconv.Itoa(n) }
func b64(b []byte) string { return base64.RawStdEncoding.EncodeToString(b) }

func TestHashAndVerifyRoundTrip(t *testing.T) {
	const pw = "correct horse battery staple"

	encoded, err := Hash(pw)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	ok, needsRehash, err := Verify(pw, encoded)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatal("a password did not verify against its own hash")
	}
	// Freshly hashed with the current parameters, so there is nothing to
	// upgrade. Without this the needsRehash test below would pass against an
	// implementation that always reports true.
	if needsRehash {
		t.Error("a hash made with the current parameters was reported as needing a rehash")
	}

	if ok, _, _ := Verify("wrong", encoded); ok {
		t.Error("the wrong password verified")
	}
}

// The stored hash reveals nothing about the password, and carries what is
// needed to verify it later.
func TestHashFormat(t *testing.T) {
	const pw = "a-distinctive-password-string"

	encoded, err := Hash(pw)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	if strings.Contains(encoded, pw) {
		t.Fatal("the password is in its own hash")
	}
	// The parameters travel with the digest, which is what makes raising them
	// later a constant change rather than a migration.
	for _, want := range []string{"$argon2id$", "m=", "t=", "p=", "v="} {
		if !strings.Contains(encoded, want) {
			t.Errorf("the encoding has no %q, so it cannot describe how it was made: %s", want, encoded)
		}
	}

	// Two hashes of the same password differ, because the salt does. Without a
	// per-hash salt, identical passwords are visibly identical in the table.
	second, err := Hash(pw)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if encoded == second {
		t.Fatal("hashing the same password twice produced the same string, so the salt is not random")
	}
}

// A hash made with weaker parameters still verifies, and asks to be rewritten.
//
// This is the property that lets the cost constants move. Without it, raising
// them either locks every existing account out or silently leaves them on the
// old cost forever — and the second is the one that happens quietly.
func TestAWeakerHashVerifiesAndAsksToBeRehashed(t *testing.T) {
	const pw = "an-older-password"

	// Built by hand at parameters below the current ones, which is what a hash
	// written by a previous build looks like.
	weak := encodeAt(t, pw, argonMemory/2, 1, 1)

	ok, needsRehash, err := Verify(pw, weak)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatal("a hash made with older parameters failed to verify, which would " +
			"lock out every account that predates a cost increase")
	}
	if !needsRehash {
		t.Error("a hash made with older parameters was not flagged for rewriting, " +
			"so raising the cost would never take effect on existing accounts")
	}

	// And the wrong password against the weaker hash still fails — the
	// parameters are read from the hash, not assumed.
	if ok, _, _ := Verify("wrong", weak); ok {
		t.Error("the wrong password verified against a weaker hash")
	}
}

// A hash this package cannot read is reported as such, not as a wrong password.
//
// The distinction matters: a corrupt row treated as a failed sign-in leaves an
// account unable to authenticate with nothing saying why, and the person
// debugging it starts by asking whether the user typed their password right.
func TestMalformedHashesAreReportedAsMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":              "",
		"not PHC":            "hunter2",
		"too few fields":     "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA",
		"wrong algorithm":    "$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"unparseable params": "$argon2id$v=19$memory=19456$c2FsdA$aGFzaA",
		"bad base64 salt":    "$argon2id$v=19$m=19456,t=2,p=1$!!!$aGFzaA",
		"empty salt":         "$argon2id$v=19$m=19456,t=2,p=1$$aGFzaA",
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			ok, _, err := Verify("anything", encoded)
			if ok {
				t.Fatal("a malformed hash verified")
			}
			if !errors.Is(err, ErrMalformedHash) {
				t.Errorf("got %v, want ErrMalformedHash", err)
			}
		})
	}
}

// A hash from a different argon2 version says so.
//
// Verifying against it would compute a different digest from the same inputs
// and fail every time, which presents as "my password stopped working".
func TestAForeignArgon2VersionIsRefusedByName(t *testing.T) {
	encoded := "$argon2id$v=16$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2E$aGFzaGhhc2hoYXNoaGFzaA"

	_, _, err := Verify("anything", encoded)
	if !errors.Is(err, ErrMalformedHash) {
		t.Fatalf("got %v, want ErrMalformedHash", err)
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("the error does not mention the version mismatch: %v", err)
	}
}

// encodeAt builds a PHC string at chosen parameters, for testing the upgrade
// path. It deliberately mirrors Hash rather than calling it, because the point
// is to produce something Hash would not.
func encodeAt(t *testing.T, password string, memory, time uint32, threads uint8) string {
	t.Helper()
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(password), salt, time, memory, threads, argonKeyLen)
	return "$argon2id$v=" + itoa(argon2.Version) +
		"$m=" + itoa(int(memory)) + ",t=" + itoa(int(time)) + ",p=" + itoa(int(threads)) +
		"$" + b64(salt) + "$" + b64(key)
}
