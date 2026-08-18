package authn

import (
	"crypto/rand"
	"fmt"
	"strings"
)

// BackupCodeCount is how many are issued at once.
//
// Ten is the industry convention and it is a reasonable one: enough that losing
// a phone is survivable more than once, few enough to print on a card.
const BackupCodeCount = 10

// Backup-code shape.
//
// Ten characters from a 32-symbol alphabet is fifty bits. That is far below a
// password's entropy, and it is why these are hashed with argon2id rather than
// SHA-256 — fifty bits against a fast hash is reachable with a GPU, and against
// a memory-hard one it is not.
//
// The alphabet excludes I, L, O, U and 0, 1: the pairs a person misreads off a
// printed card, plus the letter that makes codes spell things. The cost is a
// slightly smaller alphabet; the benefit is that "it says my code is wrong"
// support requests are about the code and not about the font.
const (
	backupCodeLen      = 10
	backupCodeAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"
)

// NewBackupCodes returns fresh codes in their display form, and the argon2id
// hashes to store.
//
// The codes are returned once and never again — the caller shows them and the
// database keeps only the hashes, which is the whole point.
func NewBackupCodes() (codes []string, hashes []string, err error) {
	for range BackupCodeCount {
		code, err := newBackupCode()
		if err != nil {
			return nil, nil, err
		}
		h, err := Hash(NormalizeBackupCode(code))
		if err != nil {
			return nil, nil, fmt.Errorf("hash backup code: %w", err)
		}
		codes = append(codes, code)
		hashes = append(hashes, h)
	}
	return codes, hashes, nil
}

func newBackupCode() (string, error) {
	b := make([]byte, backupCodeLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate backup code: %w", err)
	}
	out := make([]byte, backupCodeLen)
	for i, v := range b {
		// Modulo bias: the alphabet is 30 symbols and the byte range is 256, so
		// the first 16 symbols are very slightly more likely. That costs a
		// fraction of a bit against fifty, and the alternative — rejection
		// sampling — is more code for a difference no attacker can use.
		out[i] = backupCodeAlphabet[int(v)%len(backupCodeAlphabet)]
	}
	// Grouped for reading aloud and typing back.
	return string(out[:5]) + "-" + string(out[5:]), nil
}

// NormalizeBackupCode folds a code the way a user might type it.
//
// Hyphens, spaces and case are display, not content. Without this a user who
// omits the hyphen — which the grouping invites — is told their correct code is
// wrong, and has no way to discover why.
func NormalizeBackupCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if strings.ContainsRune(backupCodeAlphabet, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
