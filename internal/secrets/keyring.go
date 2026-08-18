// Package secrets encrypts secrets that have to be stored reversibly.
//
// # Who uses it
//
// Two processes, which is why it lives here rather than under either of them:
//
//   - The console seals one client private key per organisation in
//     console.orgs, so that dialling the wrong organisation's atlantis fails at
//     the TLS handshake rather than returning somebody else's data.
//   - Cloud seals each account's TOTP secret in cloud.totp_secrets. A second
//     factor must be recomputable, so unlike the argon2id password hash beside
//     it, it cannot be hashed.
//
// Both are the same shape of problem: a value that cannot be one-way hashed
// because it has to be used again, living in a database that gets backed up,
// replicated, and read by anything that can reach it.
//
// # What this defends, and what it does not
//
// **A database dump alone yields nothing.** A leaked backup, a replica, a broad
// SELECT over either table — none of them produce a usable key or a working
// second factor, because the key-encrypting key is not in the database.
//
// It does **not** defend a compromised process. Each of them must be able to
// decrypt in order to serve anybody, so the key-encrypting key is in memory by
// necessity. Nothing at this layer changes that, and saying otherwise would be
// worse than the gap itself — an operator who believes the secrets are safe
// from a compromised process would make different decisions about how that
// process is run.
//
// # Why Tink rather than crypto/cipher
//
// The parts of authenticated encryption that go wrong are quiet: a reused
// nonce, a mismatched key type, a ciphertext framing that loses the key id.
// Tink does not expose those choices.
//
// Rotation is the other reason, and it is the one that shows up in the schema.
// A Tink keyset holds several keys at once: the primary encrypts, and any
// member can decrypt, with the key id carried in the ciphertext prefix. Adding
// a key and promoting it rotates everything forward with no re-encryption pass
// and no key-version column — the ciphertext says which key it needs.
package secrets

import (
	"bytes"
	"encoding/base64"
	"fmt"

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
	"github.com/tink-crypto/tink-go/v2/tink"
)

// Keyring encrypts and decrypts a secret bound to a context.
//
// associatedData is authenticated but not encrypted: the ciphertext can only be
// opened by supplying the same value it was sealed with. Callers pass the
// organisation the secret belongs to, which is what stops a stored key being
// moved between rows — see the note on Encrypt.
//
// An interface, and a small one, because the implementation is expected to
// change. Today the keyset arrives in the environment; when Cloud runs on GCP
// it will arrive wrapped by Cloud KMS. That is a different constructor and the
// same three methods, so neither the call sites nor the schema move.
type Keyring interface {
	Encrypt(plaintext, associatedData []byte) ([]byte, error)
	Decrypt(ciphertext, associatedData []byte) ([]byte, error)
}

// tinkKeyring is a Keyring over a Tink AEAD primitive.
type tinkKeyring struct{ aead tink.AEAD }

// FromEnvKeyset builds a Keyring from a base64-encoded Tink keyset.
//
// The keyset is binary-serialised and read through insecurecleartextkeyset —
// "insecure" naming the one true thing about it, which is that the keyset is
// not itself wrapped by a key management service. That is the property the KMS
// implementation changes, and it is why the import is deliberately hard to read
// past.
//
// Refuses an empty value rather than defaulting to anything. A console that
// generated its own key on first boot would encrypt every organisation's
// credentials under a key that vanishes with the container, which presents as
// every organisation being unreachable after a restart — with the data intact
// and permanently unopenable.
func FromEnvKeyset(encoded string) (Keyring, error) {
	if encoded == "" {
		return nil, fmt.Errorf("no keyset: the console cannot store or read " +
			"organisation credentials without one")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("keyset is not valid base64: %w", err)
	}
	handle, err := insecurecleartextkeyset.Read(keyset.NewBinaryReader(bytes.NewReader(raw)))
	if err != nil {
		return nil, fmt.Errorf("keyset could not be read: %w", err)
	}
	primitive, err := aead.New(handle)
	if err != nil {
		// The usual cause is a keyset of the wrong primitive type — a signing
		// keyset where an AEAD one belongs. Worth saying so, because the
		// underlying error names an interface rather than a mistake.
		return nil, fmt.Errorf("keyset does not provide authenticated encryption "+
			"(is it an AEAD keyset?): %w", err)
	}
	return &tinkKeyring{aead: primitive}, nil
}

// Encrypt seals plaintext against associatedData.
//
// The associated data is the load-bearing argument. It is authenticated but not
// stored in the ciphertext, so opening the result requires supplying the same
// value again — and callers supply the organisation name. That converts a write
// primitive into a non-attack: somebody who can UPDATE console.orgs cannot lift
// one organisation's encrypted key onto another's row, because the row they
// move it to decrypts under a different organisation and the open fails.
//
// Without it, the same UPDATE hands the attacker a working credential for
// whichever organisation they copied from, and every layer downstream —
// including the per-organisation CA this whole step rests on — is satisfied,
// because the credential is genuine. It is simply being used by the wrong
// tenant.
func (k *tinkKeyring) Encrypt(plaintext, associatedData []byte) ([]byte, error) {
	if len(associatedData) == 0 {
		// Fails closed rather than sealing against nothing. An empty context
		// would encrypt correctly and open from any row, which is precisely the
		// property the argument exists to deny — and it would look like it was
		// working.
		return nil, fmt.Errorf("refusing to encrypt with no associated data: " +
			"the ciphertext would open against any organisation")
	}
	return k.aead.Encrypt(plaintext, associatedData)
}

// Decrypt opens ciphertext, which requires the associatedData it was sealed
// with.
func (k *tinkKeyring) Decrypt(ciphertext, associatedData []byte) ([]byte, error) {
	if len(associatedData) == 0 {
		return nil, fmt.Errorf("refusing to decrypt with no associated data")
	}
	return k.aead.Decrypt(ciphertext, associatedData)
}

// NewKeyset mints a fresh AES-256-GCM keyset, base64-encoded in the form
// FromEnvKeyset accepts.
//
// Not a test helper, despite being what the tests use. `cloud data-key` calls
// this to produce the value an operator exports as CONSOLE_DATA_KEY, and it is
// deliberately the same code path, so a keyset that works in a test is the same
// artefact that works in a deployment.
//
// It mints a keyset with one key in it. Rotation adds a second and promotes it,
// which is a keyset operation rather than a data migration — see the package
// comment. Nothing here does that yet, because nothing has needed to; the
// ciphertext already carries the key id that will make it work.
func NewKeyset() (string, error) {
	handle, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	if err != nil {
		return "", fmt.Errorf("generate keyset: %w", err)
	}
	var buf bytes.Buffer
	if err := insecurecleartextkeyset.Write(handle, keyset.NewBinaryWriter(&buf)); err != nil {
		return "", fmt.Errorf("serialise keyset: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
