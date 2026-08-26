package main

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The credential store: what `tide login` writes and every later command reads.
//
//	~/.atlantis/<org>/<caller>/
//	    client.pem    private key + leaf, 0600
//	    ca.crt        the root that verifies this organisation's atlantis
//	    endpoint      the address callers dial
//	    enroll_url    where to renew
//
// Keyed by organisation and caller. One developer with two repositories in one
// organisation is ordinary, and keying by organisation alone would make the
// second `tide login` overwrite a key whose certificate is still live.
//
// The key and the leaf share one file, because renewal replaces them together.
// As two files, a crash can pair a new certificate with an old key, and
// tls.LoadX509KeyPair then fails with "private key does not match public key",
// which running login again cannot fix because it refuses to overwrite a key.
// One file is a single atomic rename.
//
// ca.crt and endpoint are not rewritten by renewal in the ordinary case, so
// they carry no such requirement.

// storeRoot is the directory the store lives under. Overridable, for tests and
// for an unusual home directory.
func storeRoot() (string, error) {
	if v := os.Getenv("ATLANTIS_HOME"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate your home directory: %w", err)
	}
	return filepath.Join(home, ".atlantis"), nil
}

// validStoreName is the grammar for a path component in the store.
//
// The same one atlantis enforces for caller names (admin.validCallerName), and
// applied here to the organisation too. Both arrive from outside — the
// organisation from a flag, the caller from a server response — and both are
// joined into a filesystem path, so `../..` has to be impossible rather than
// merely unlikely.
func validStoreName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}

// credDir is where one caller's credentials live.
func credDir(org, caller string) (string, error) {
	if !validStoreName(org) {
		return "", fmt.Errorf("%q is not a valid organisation name "+
			"(lowercase letters, digits and interior hyphens)", org)
	}
	if !validStoreName(caller) {
		return "", fmt.Errorf("%q is not a valid caller name "+
			"(lowercase letters, digits and interior hyphens)", caller)
	}
	root, err := storeRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, org, caller), nil
}

// storedCredentials is one caller's material, as read back off disk.
type storedCredentials struct {
	Org, Caller string
	Dir         string

	// ClientPEM holds the key and the leaf together. tls.X509KeyPair is happy
	// to be handed the same buffer twice: it scans for a CERTIFICATE block and
	// for a block whose type contains "PRIVATE KEY", so one file serves both.
	ClientPEM []byte
	CAPEM     []byte

	Endpoint  string
	EnrollURL string

	// NotAfter comes from the certificate rather than from a file beside it.
	// A stored copy is a second source that can disagree with the thing it
	// describes, and renewal decides what to do from exactly this value.
	NotAfter time.Time
}

const (
	clientPEMName = "client.pem"
	caPEMName     = "ca.crt"
	endpointName  = "endpoint"
	enrollURLName = "enroll_url"
)

// ErrNoCredentials reports that nothing has been enrolled for this pair.
var ErrNoCredentials = errors.New("no credentials in the store")

// loadCredentials reads one caller's material.
func loadCredentials(org, caller string) (*storedCredentials, error) {
	dir, err := credDir(org, caller)
	if err != nil {
		return nil, err
	}
	clientPEM, err := os.ReadFile(filepath.Join(dir, clientPEMName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(dir, clientPEMName), err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, caPEMName))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(dir, caPEMName), err)
	}

	c := &storedCredentials{
		Org: org, Caller: caller, Dir: dir,
		ClientPEM: clientPEM, CAPEM: caPEM,
		Endpoint:  readTrimmed(dir, endpointName),
		EnrollURL: readTrimmed(dir, enrollURLName),
	}
	leaf, err := leafOf(clientPEM)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, clientPEMName), err)
	}
	c.NotAfter = leaf.NotAfter
	return c, nil
}

func readTrimmed(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// leafOf pulls the certificate out of a combined key+certificate file.
func leafOf(clientPEM []byte) (*x509.Certificate, error) {
	rest := clientPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("holds no certificate")
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
	}
}

// writeNewCredentials creates a store entry, refusing to replace one.
//
// Opened O_EXCL. The key in an existing entry belongs to a certificate that is
// probably still live at atlantis, so overwriting it would leave the machine
// holding material for an identity it can no longer prove.
//
// Renewal uses replaceClientPEM below, which is meant to replace, has just
// proved possession of the certificate it replaces, and writes by rename.
func writeNewCredentials(c *storedCredentials) error {
	dir, err := credDir(c.Org, c.Caller)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone, so a store created
	// before this — or by something with a laxer umask — would keep whatever it
	// had. Asserting the mode is the point; inheriting it is not.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure %s: %w", dir, err)
	}

	f, err := os.OpenFile(filepath.Join(dir, clientPEMName),
		os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s already holds credentials for %s/%s.\n\n"+
			"Its certificate is probably still valid at atlantis, and replacing the "+
			"key would leave this machine unable to prove an identity it still has. "+
			"Renewal happens on its own; to start over, remove the directory.",
			dir, c.Org, c.Caller)
	}
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Join(dir, clientPEMName), err)
	}
	if _, err := f.Write(c.ClientPEM); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", filepath.Join(dir, clientPEMName), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Join(dir, clientPEMName), err)
	}

	return writeSidecars(dir, c)
}

// writeSidecars writes everything that is not the key.
//
// 0644: a CA certificate and two addresses are public by construction. Only
// client.pem is a secret, and it is the one file written 0600.
func writeSidecars(dir string, c *storedCredentials) error {
	for name, body := range map[string][]byte{
		caPEMName:     c.CAPEM,
		endpointName:  []byte(c.Endpoint + "\n"),
		enrollURLName: []byte(c.EnrollURL + "\n"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", filepath.Join(dir, name), err)
		}
	}
	return nil
}

// replaceClientPEM swaps in renewed material atomically.
//
// Write-then-rename, because rename within a directory is atomic: a reader sees
// either the old file or the new one, never a half-written one and never a
// certificate paired with the wrong key. That is the whole reason the key and
// the certificate share a file.
//
// The temporary lands in the same directory so the rename cannot cross a
// filesystem boundary, which is the one thing that would make it non-atomic.
func replaceClientPEM(dir string, clientPEM []byte) error {
	tmp := filepath.Join(dir, clientPEMName+".tmp")
	if err := os.WriteFile(tmp, clientPEM, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, clientPEMName)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", filepath.Join(dir, clientPEMName), err)
	}
	return nil
}

// callersInStore lists the callers enrolled for an organisation.
//
// Resolves `caller` when a repository names none, and fills the error when a
// lookup misses.
func callersInStore(org string) ([]string, error) {
	root, err := storeRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(root, org))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// orgsInStore lists the organisations with at least one enrolment.
func orgsInStore() ([]string, error) {
	root, err := storeRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}
