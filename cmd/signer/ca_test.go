package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/cloud/provision/certs"
)

// The seam between the thing that mints a certificate authority and the thing
// that boots from one.
//
// These live here rather than in the certs package because that is where the
// defect was: certs.Generate emits PKCS#8, loadCA parsed only SEC 1, and the
// certs package's own suite was green throughout — every assertion there went
// through tls.X509KeyPair, which takes either encoding and therefore could not
// see it. A test that only exercises the lenient reader cannot catch a strict
// one, so this one calls the strict reader directly.

func writeCA(t *testing.T, dir string, a certs.Authority) {
	t.Helper()
	// loadCA reads exactly these two names.
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), a.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), a.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The regression. Provisioning mints an authority with certs.Generate and the
// signer has to boot from it; before parseECPrivateKey existed, every signer
// pod exited 1 with `parse /ca-private/ca.key`.
func TestTheSignerLoadsAnAuthorityMintedByProvisioning(t *testing.T) {
	bundle, err := certs.Generate(certs.Options{
		Org:            "acme",
		ServerDNSNames: []string{"atlantis.acme.svc"},
		SignerDNSNames: []string{"signer.acme.svc"},
	})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	writeCA(t, dir, bundle.CA)

	cert, key, crtPEM, err := loadCA(dir)
	if err != nil {
		t.Fatalf("loadCA on a provisioned authority: %v", err)
	}
	if cert == nil || key == nil || len(crtPEM) == 0 {
		t.Fatal("loadCA returned no error and no material")
	}
	// The pair has to actually belong together, or the signer would mint leaves
	// nothing chains to — a failure that surfaces at a caller's handshake
	// rather than here.
	if !key.PublicKey.Equal(cert.PublicKey) {
		t.Error("the loaded key does not match the loaded certificate")
	}
	// It must be the issuing authority, not one of the leaves.
	if !cert.IsCA {
		t.Error("loadCA accepted a certificate that is not a CA")
	}
}

// The encoding openssl produces, which is what deploy/init-certs.sh writes and
// what every certificate issued before the Go generator existed carries.
// Accepting PKCS#8 must not have cost us this.
func TestTheSignerStillLoadsASEC1Authority(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sec1, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	bundle, err := certs.Generate(certs.Options{
		Org:            "acme",
		ServerDNSNames: []string{"atlantis.acme.svc"},
		SignerDNSNames: []string{"signer.acme.svc"},
	})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	// The certificate does not match this key, which loadCA does not check and
	// is not what this test is about — the assertion is only that a SEC 1 block
	// still parses.
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), bundle.CA.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	sec1PEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), sec1PEM, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := loadCA(dir); err != nil {
		t.Fatalf("loadCA no longer accepts a SEC 1 key: %v", err)
	}
}

func mustRSA(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	// 1024 bits: this key is never used for anything, and a realistic size
	// costs a noticeable fraction of a second on every run of the suite.
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestParseECPrivateKeyRefusesWhatItCannotUse(t *testing.T) {
	if _, err := parseECPrivateKey([]byte("not a key")); err == nil {
		t.Error("parseECPrivateKey accepted arbitrary bytes")
	}

	// An RSA key in PKCS#8 parses successfully as a key and is still wrong
	// here. Without the type assertion this would return a nil *ecdsa.PrivateKey
	// and fail much later, at signing time, with an error about the signature
	// rather than about the key.
	rsaLike, err := x509.MarshalPKCS8PrivateKey(mustRSA(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseECPrivateKey(rsaLike); err == nil {
		t.Error("parseECPrivateKey accepted an RSA key")
	}
}
