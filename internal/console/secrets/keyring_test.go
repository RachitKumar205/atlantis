package secrets

import (
	"bytes"
	"strings"
	"testing"
)

func newTestKeyring(t *testing.T) Keyring {
	t.Helper()
	encoded, err := NewKeyset()
	if err != nil {
		t.Fatalf("NewKeyset: %v", err)
	}
	k, err := FromEnvKeyset(encoded)
	if err != nil {
		t.Fatalf("FromEnvKeyset: %v", err)
	}
	return k
}

// The control. Without it every negative case below is satisfied by a Keyring
// that fails unconditionally.
func TestRoundTrip(t *testing.T) {
	k := newTestKeyring(t)
	secret := []byte("-----BEGIN EC PRIVATE KEY-----\nnot really a key\n")

	ct, err := k.Encrypt(secret, []byte("acme"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := k.Decrypt(ct, []byte("acme"))
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Errorf("round trip changed the secret")
	}
}

// TestCiphertextDoesNotContainThePlaintext is the property the whole package
// exists for, asserted on the bytes that actually reach the database rather
// than on the fact that a function named Encrypt was called.
func TestCiphertextDoesNotContainThePlaintext(t *testing.T) {
	k := newTestKeyring(t)
	secret := []byte("-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIBoggle\n-----END EC PRIVATE KEY-----")

	ct, err := k.Encrypt(secret, []byte("acme"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Contains(ct, secret) {
		t.Fatal("the ciphertext contains the plaintext verbatim")
	}
	if bytes.Contains(ct, []byte("BEGIN EC PRIVATE KEY")) {
		t.Error("the ciphertext contains a PEM header; a dump would be recognisably a key")
	}
}

// TestCiphertextCannotBeMovedBetweenOrganisations is the associated-data
// binding, and it is the reason associatedData is an argument at all.
//
// Somebody who can UPDATE console.orgs can copy a column between rows. Without
// this binding that hands them a genuine, working credential for whichever
// organisation they copied from — and every check downstream is satisfied,
// because the credential is real. It is only being used by the wrong tenant.
func TestCiphertextCannotBeMovedBetweenOrganisations(t *testing.T) {
	k := newTestKeyring(t)
	secret := []byte("acme's private key")

	ct, err := k.Encrypt(secret, []byte("acme"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Opening it as acme works — so the failure below is caused by the
	// organisation and not by a broken ciphertext.
	if _, err := k.Decrypt(ct, []byte("acme")); err != nil {
		t.Fatalf("the control decrypt failed, so the case below proves nothing: %v", err)
	}

	if _, err := k.Decrypt(ct, []byte("globex")); err == nil {
		t.Fatal("acme's key opened on globex's row")
	}
}

func TestEmptyAssociatedDataIsRefused(t *testing.T) {
	k := newTestKeyring(t)

	// Sealing against nothing would produce a ciphertext that opens from any
	// row — the exact property the argument denies — and it would look like it
	// worked.
	if _, err := k.Encrypt([]byte("secret"), nil); err == nil {
		t.Error("encrypted with no associated data")
	}
	if _, err := k.Encrypt([]byte("secret"), []byte("")); err == nil {
		t.Error("encrypted with empty associated data")
	}

	ct, err := k.Encrypt([]byte("secret"), []byte("acme"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := k.Decrypt(ct, nil); err == nil {
		t.Error("decrypted with no associated data")
	}
}

func TestTamperedCiphertextIsRefused(t *testing.T) {
	k := newTestKeyring(t)
	ct, err := k.Encrypt([]byte("acme's private key"), []byte("acme"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Flip a bit in the body, past Tink's key-id prefix.
	tampered := bytes.Clone(ct)
	tampered[len(tampered)-1] ^= 0x01

	if _, err := k.Decrypt(tampered, []byte("acme")); err == nil {
		t.Error("a modified ciphertext decrypted; this is authenticated encryption or it is nothing")
	}
}

// TestAKeysetFromElsewhereCannotOpenIt. Two consoles, or one console before and
// after a key was replaced, must not read each other's stored credentials.
func TestAKeysetFromElsewhereCannotOpenIt(t *testing.T) {
	mine := newTestKeyring(t)
	theirs := newTestKeyring(t)

	ct, err := mine.Encrypt([]byte("acme's private key"), []byte("acme"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := theirs.Decrypt(ct, []byte("acme")); err == nil {
		t.Error("a different keyset opened the ciphertext")
	}
}

func TestFromEnvKeysetRefusesRubbish(t *testing.T) {
	for _, tc := range []struct {
		name, keyset, want string
	}{
		{"empty", "", "without one"},
		{"not base64", "!!!not base64!!!", "base64"},
		{"base64 but not a keyset", "aGVsbG8gd29ybGQ=", "could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FromEnvKeyset(tc.keyset)
			if err == nil {
				t.Fatal("accepted something that is not a usable keyset")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q — it was refused, but not for the reason this case is about", err, tc.want)
			}
		})
	}

	// The counterpart, without which the table above passes against a
	// constructor that rejects everything.
	encoded, err := NewKeyset()
	if err != nil {
		t.Fatalf("NewKeyset: %v", err)
	}
	if _, err := FromEnvKeyset(encoded); err != nil {
		t.Fatalf("FromEnvKeyset rejected a keyset it had just generated: %v", err)
	}
}

// TestEveryCiphertextDiffers catches the worst kind of mistake this package
// could make: a deterministic construction, or a reused nonce, either of which
// leaks that two organisations share a key and is invisible from the outside.
func TestEveryCiphertextDiffers(t *testing.T) {
	k := newTestKeyring(t)
	secret := []byte("the same secret every time")

	seen := make(map[string]bool)
	for range 50 {
		ct, err := k.Encrypt(secret, []byte("acme"))
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if seen[string(ct)] {
			t.Fatal("the same plaintext encrypted to the same ciphertext twice")
		}
		seen[string(ct)] = true
	}
}
