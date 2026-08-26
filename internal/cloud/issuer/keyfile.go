package issuer

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

// keyPEMType is the PEM block label for a PKCS#8 private key.
const keyPEMType = "PRIVATE KEY"

// LoadOrCreateKey reads the signing key at path, generating and writing one if
// the file does not exist. It reports whether it created the key.
//
// The JWKS is served from this process, so a key generated per process changes
// the published key set on every restart: assertions issued before it stop
// verifying, and consoles holding a cached set verify against a key that is
// gone.
func LoadOrCreateKey(path string) (*Key, bool, error) {
	pemBytes, err := os.ReadFile(path)
	if err == nil {
		k, err := parseKey(pemBytes, path)
		return k, false, err
	}
	if !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("read signing key %s: %w", path, err)
	}

	k, err := GenerateKey()
	if err != nil {
		return nil, false, err
	}
	if err := writeKey(path, k.priv); err != nil {
		return nil, false, err
	}
	return k, true, nil
}

// parseKey decodes a PEM private key and rebuilds its kid.
//
// The kid is derived from the key rather than stored beside it, so a key file
// carries no metadata that could disagree with the key it contains.
func parseKey(pemBytes []byte, path string) (*Key, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("signing key %s is not PEM", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse signing key %s: %w", path, err)
	}
	priv, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("signing key %s is %T, want an ECDSA key", path, parsed)
	}

	k := &Key{priv: priv}
	id, err := thumbprintOf(k)
	if err != nil {
		return nil, err
	}
	k.ID = id
	return k, nil
}

func writeKey(path string, priv *ecdsa.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("encode signing key: %w", err)
	}

	// 0700 on the directory, 0600 on the file. This key mints an assertion for
	// any user in any organisation, and no downstream check distinguishes one
	// from a legitimate assertion.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create key directory %s: %w", dir, err)
		}
	}

	// O_EXCL, so a key that appeared between the read above and this write is
	// not overwritten, which would invalidate every assertion in flight.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create signing key %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if err := pem.Encode(f, &pem.Block{Type: keyPEMType, Bytes: der}); err != nil {
		return fmt.Errorf("write signing key %s: %w", path, err)
	}
	return f.Close()
}
