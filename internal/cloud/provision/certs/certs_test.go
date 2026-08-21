package certs

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"
	"time"
)

func generate(t *testing.T, org string) *Bundle {
	t.Helper()
	b, err := Generate(Options{
		Org:            org,
		ServerDNSNames: []string{"atlantis." + org + ".svc.cluster.local", "atl-dev.test"},
		ServerIPs:      []net.IP{net.ParseIP("127.0.0.1")},
		SignerDNSNames: []string{"signer." + org + ".svc.cluster.local"},
	})
	if err != nil {
		t.Fatalf("Generate(%q): %v", org, err)
	}
	return b
}

func parseLeaf(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		t.Fatal("no PEM block")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

func pool(t *testing.T, caPEM []byte) *x509.CertPool {
	t.Helper()
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(caPEM) {
		t.Fatal("CA PEM did not parse")
	}
	return p
}

// verifies reports whether leaf chains to root for the given usage. Both
// answers are interesting here — most of these tests are about a chain that
// must NOT build.
func verifies(t *testing.T, leafPEM, rootPEM []byte, usage x509.ExtKeyUsage) bool {
	t.Helper()
	_, err := parseLeaf(t, leafPEM).Verify(x509.VerifyOptions{
		Roots:     pool(t, rootPEM),
		KeyUsages: []x509.ExtKeyUsage{usage},
	})
	return err == nil
}

// The bundle is useless if the pieces do not chain where they are meant to, so
// assert that before asserting what must fail. A test suite that only checks
// refusals passes against a generator that produces nothing at all.
func TestEachLeafChainsToItsOwnRoot(t *testing.T) {
	b := generate(t, "acme")

	for _, tc := range []struct {
		name  string
		leaf  []byte
		root  []byte
		usage x509.ExtKeyUsage
	}{
		{"atlantis server", b.Server.CertPEM, b.CA.CertPEM, x509.ExtKeyUsageServerAuth},
		{"console client", b.Console.CertPEM, b.CA.CertPEM, x509.ExtKeyUsageClientAuth},
		{"signer server", b.SignerServer.CertPEM, b.SignerCA.CertPEM, x509.ExtKeyUsageServerAuth},
		{"signer client", b.SignerClient.CertPEM, b.SignerCA.CertPEM, x509.ExtKeyUsageClientAuth},
	} {
		if !verifies(t, tc.leaf, tc.root, tc.usage) {
			t.Errorf("%s does not chain to its own root", tc.name)
		}
	}
}

// The invariant init-certs.sh spends twenty lines on: nothing is signed by both
// roots. If the signer verified its clients against the authority it issues
// from, every certificate it had ever issued would also be a credential for
// talking to it.
func TestTheTwoAuthoritiesAreSeparateTrustDomains(t *testing.T) {
	b := generate(t, "acme")

	if verifies(t, b.SignerClient.CertPEM, b.CA.CertPEM, x509.ExtKeyUsageClientAuth) {
		t.Error("the signer's client certificate chains to the ISSUING CA; " +
			"the two authorities have been merged and any caller certificate " +
			"is now also a credential for the signer")
	}
	if verifies(t, b.Console.CertPEM, b.SignerCA.CertPEM, x509.ExtKeyUsageClientAuth) {
		t.Error("the console's atlantis certificate chains to the SIGNER CA")
	}
	if verifies(t, b.Server.CertPEM, b.SignerCA.CertPEM, x509.ExtKeyUsageServerAuth) {
		t.Error("the atlantis server leaf chains to the SIGNER CA")
	}

	caCN := parseLeaf(t, b.CA.CertPEM).Subject.CommonName
	signerCN := parseLeaf(t, b.SignerCA.CertPEM).Subject.CommonName
	if caCN == signerCN {
		t.Errorf("both roots have common name %q", caCN)
	}
}

// The reason every organisation gets its own roots. Caller names are chosen by
// customers, identity at the server is the common name and nothing else, so
// "backend" will exist in more than one organisation. This is the check that
// makes the collision harmless.
func TestOneOrganisationsCertificateIsRefusedByAnother(t *testing.T) {
	a := generate(t, "acme")
	c := generate(t, "contoso")

	if verifies(t, a.Console.CertPEM, c.CA.CertPEM, x509.ExtKeyUsageClientAuth) {
		t.Fatal("acme's console certificate is accepted by contoso's atlantis: " +
			"the organisations share a trust root")
	}
	if verifies(t, c.Console.CertPEM, a.CA.CertPEM, x509.ExtKeyUsageClientAuth) {
		t.Fatal("contoso's console certificate is accepted by acme's atlantis")
	}

	// Same common name in both, which is the point: the name collides and the
	// certificate still does not.
	if got := parseLeaf(t, a.Console.CertPEM).Subject.CommonName; got != ConsoleCN {
		t.Errorf("acme console CN = %q, want %q", got, ConsoleCN)
	}
	if got := parseLeaf(t, c.Console.CertPEM).Subject.CommonName; got != ConsoleCN {
		t.Errorf("contoso console CN = %q, want %q", got, ConsoleCN)
	}
}

// internal/console/client.go leaves tls.Config.ServerName unset on purpose, so
// the leaf has to match whatever address was dialled — and there are two of
// them, the in-cluster one and the public one.
func TestTheServerLeafMatchesEveryAddressItIsDialledBy(t *testing.T) {
	b := generate(t, "acme")
	leaf := parseLeaf(t, b.Server.CertPEM)

	for _, name := range []string{"atlantis.acme.svc.cluster.local", "atl-dev.test"} {
		if err := leaf.VerifyHostname(name); err != nil {
			t.Errorf("server leaf does not match %q: %v", name, err)
		}
	}
	if err := leaf.VerifyHostname("127.0.0.1"); err != nil {
		t.Errorf("server leaf does not match 127.0.0.1: %v", err)
	}
	if err := leaf.VerifyHostname("atlantis.other.svc.cluster.local"); err == nil {
		t.Error("server leaf matches a name it was never given")
	}
}

// Apple's verifier rejects a TLS server certificate valid for more than 825
// days, and says so with a message that names one of its own rules rather than
// anything about trust. Verified at the boundary when the shell script was
// shortened; this keeps the Go path from drifting back over the line.
func TestServerCertificatesStayUnderTheAppleCeiling(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	b, err := Generate(Options{
		Org:            "acme",
		ServerDNSNames: []string{"atlantis.acme.svc"},
		SignerDNSNames: []string{"signer.acme.svc"},
		Now:            now,
	})
	if err != nil {
		t.Fatal(err)
	}

	const ceiling = 825 * 24 * time.Hour
	for _, tc := range []struct {
		name string
		pem  []byte
	}{
		{"atlantis server", b.Server.CertPEM},
		{"signer server", b.SignerServer.CertPEM},
	} {
		leaf := parseLeaf(t, tc.pem)
		if life := leaf.NotAfter.Sub(leaf.NotBefore); life >= ceiling {
			t.Errorf("%s lives %v, which is at or over the 825-day ceiling", tc.name, life)
		}
	}
}

// A certificate whose key does not match is a failure that surfaces at the
// handshake, a long way from here. internal/console/register.go pairs them with
// exactly this call before it will store an organisation's credentials.
func TestEveryCertificateIsPairedWithItsKey(t *testing.T) {
	b := generate(t, "acme")

	for _, tc := range []struct {
		name string
		leaf Leaf
	}{
		{"server", b.Server},
		{"console", b.Console},
		{"signer server", b.SignerServer},
		{"signer client", b.SignerClient},
	} {
		if _, err := tls.X509KeyPair(tc.leaf.CertPEM, tc.leaf.KeyPEM); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	for _, tc := range []struct {
		name string
		a    Authority
	}{
		{"CA", b.CA},
		{"signer CA", b.SignerCA},
	} {
		if _, err := tls.X509KeyPair(tc.a.CertPEM, tc.a.KeyPEM); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// Both roots must be usable as roots. A CA without BasicConstraintsValid, or
// without KeyUsageCertSign, builds no chain at all — and the resulting error
// points at the leaf rather than at the root that caused it.
func TestRootsAreUsableAsRoots(t *testing.T) {
	b := generate(t, "acme")

	for _, tc := range []struct {
		name string
		pem  []byte
	}{
		{"CA", b.CA.CertPEM},
		{"signer CA", b.SignerCA.CertPEM},
	} {
		root := parseLeaf(t, tc.pem)
		if !root.IsCA {
			t.Errorf("%s is not marked as a CA", tc.name)
		}
		if !root.BasicConstraintsValid {
			t.Errorf("%s has no basic constraints", tc.name)
		}
		if root.KeyUsage&x509.KeyUsageCertSign == 0 {
			t.Errorf("%s cannot sign certificates", tc.name)
		}
	}
}

// A server certificate presented as a client credential, or the reverse, must
// not be accepted. Go checks extended key usage during chain building, so this
// is what stops the atlantis server leaf being replayed as a caller identity.
func TestKeyUsagesAreNotInterchangeable(t *testing.T) {
	b := generate(t, "acme")

	if verifies(t, b.Server.CertPEM, b.CA.CertPEM, x509.ExtKeyUsageClientAuth) {
		t.Error("the atlantis server leaf is accepted as a client credential")
	}
	if verifies(t, b.Console.CertPEM, b.CA.CertPEM, x509.ExtKeyUsageServerAuth) {
		t.Error("the console client leaf is accepted as a server credential")
	}
}

func TestGenerateRefusesAnUnusableRequest(t *testing.T) {
	if _, err := Generate(Options{
		ServerDNSNames: []string{"x"},
		SignerDNSNames: []string{"y"},
	}); err == nil {
		t.Error("Generate accepted an empty organisation name")
	}

	// A leaf with no subject alternative name verifies against nothing, and
	// because ServerName is unset at the dialling end the failure arrives as a
	// confusing handshake error rather than as a missing SAN.
	if _, err := Generate(Options{
		Org:            "acme",
		SignerDNSNames: []string{"signer.acme.svc"},
	}); err == nil {
		t.Error("Generate accepted an atlantis leaf with no SAN")
	}

	// The signer is the one that is easy to forget, because it feels internal
	// — but the console dials it over https:// and Go checks the hostname.
	if _, err := Generate(Options{
		Org:            "acme",
		ServerDNSNames: []string{"atlantis.acme.svc"},
	}); err == nil {
		t.Error("Generate accepted a signer leaf with no SAN")
	}
}

// The signer's leaf has to match ATL_SIGNER_ADDR's host for the console to
// reach it at all. Asserted from the side that has to succeed, because the
// refusal above passes even if the SAN is dropped on the floor afterwards.
func TestTheSignerLeafMatchesTheAddressTheConsoleDials(t *testing.T) {
	b := generate(t, "acme")
	leaf := parseLeaf(t, b.SignerServer.CertPEM)

	if err := leaf.VerifyHostname("signer.acme.svc.cluster.local"); err != nil {
		t.Errorf("signer leaf does not match the address it is dialled by: %v", err)
	}
	if err := leaf.VerifyHostname("signer.other.svc.cluster.local"); err == nil {
		t.Error("signer leaf matches a name it was never given")
	}
}

// Two calls must not produce the same key material. Provisioning calls this
// once per organisation and nothing downstream would notice a repeat.
func TestEveryCallMintsFreshMaterial(t *testing.T) {
	a := generate(t, "acme")
	b := generate(t, "acme")

	if string(a.CA.KeyPEM) == string(b.CA.KeyPEM) {
		t.Fatal("two calls produced the same CA key")
	}
	if parseLeaf(t, a.CA.CertPEM).SerialNumber.Cmp(parseLeaf(t, b.CA.CertPEM).SerialNumber) == 0 {
		t.Fatal("two calls produced the same serial")
	}
	// Same organisation name, so the common names match and only the roots
	// differ — the case most likely to be mistaken for "already provisioned".
	if verifies(t, a.Console.CertPEM, b.CA.CertPEM, x509.ExtKeyUsageClientAuth) {
		t.Fatal("a certificate from one call verifies against another call's root")
	}
}
