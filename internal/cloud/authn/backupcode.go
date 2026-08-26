package authn

import (
	"crypto/rand"
	"fmt"
	"strings"
)

// BackupCodeCount is how many are issued at once.
//
// Ten: enough to survive losing a phone more than once, few enough to print.
const BackupCodeCount = 10

// Backup-code shape.
//
// Ten characters from a 32-symbol alphabet is fifty bits, which is why these
// are hashed with argon2id rather than SHA-256: fifty bits against a fast hash
// is reachable with a GPU.
//
// The alphabet excludes I, L, O, U, 0 and 1 — the pairs misread off a printed
// card, plus the letter that makes codes spell words.
const (
	backupCodeLen      = 10
	backupCodeAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"
)

// NewBackupCodes returns fresh codes in their display form, and the argon2id
// hashes to store.
//
// The codes are returned once. The database keeps only the hashes.
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
		// the first 16 symbols are slightly more likely. That costs a fraction
		// of a bit against fifty.
		out[i] = backupCodeAlphabet[int(v)%len(backupCodeAlphabet)]
	}
	// Grouped for reading aloud and typing back.
	return string(out[:5]) + "-" + string(out[5:]), nil
}

// NormalizeBackupCode folds a code the way a user might type it.
//
// Hyphens, spaces and case are display, not content. Without this, a code typed
// without its hyphen is refused as wrong.
func NormalizeBackupCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if strings.ContainsRune(backupCodeAlphabet, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
