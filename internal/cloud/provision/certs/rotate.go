package certs

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"time"
)

// Replacing the console's credentials without disturbing anything else.
//
// # Why only these two leaves
//
// The console presents a different certificate to each half of an organisation:
// Console chains to CA, which atlantis trusts for clients, and SignerClient
// chains to SignerCA, which the signer trusts for clients. They carry the same
// common name and are signed by roots that share nothing — see the package
// comment for why that separation is the whole design.
//
// Both authorities are left exactly as they are. That distinction is the
// difference between a rotation and an outage: a new root invalidates every
// caller certificate ever issued under the old one, which is why ensureCerts
// refuses to regenerate rather than self-heal. Reissuing a leaf beneath a root
// that stays put costs nothing and interrupts nobody.
//
// # Why the console cannot do this for itself
//
// It would have to ask the signer, and the signer refuses: 'atlantis-console'
// is a reserved common name precisely so that a compromised console cannot ask
// for one. The credential it would use to make the request chains to SignerCA
// besides, and nothing issues from SignerCA online — so even with the reserved
// list relaxed, the key to the door cannot be re-cut by the door. Rotation
// belongs to whoever holds the authorities, which is the provisioner.

// mintConsoleLeaves writes the two certificates the console presents into b.
//
// Shared with Generate rather than copied, so there is one definition of what
// the console's certificates are. Two copies would agree until the day one of
// them gained a field — an extended key usage, a name constraint — and the
// certificates a rotation produced would quietly stop matching the ones
// provisioning produced.
func mintConsoleLeaves(
	b *Bundle,
	caCert *x509.Certificate, caKey *ecdsa.PrivateKey,
	signerCACert *x509.Certificate, signerCAKey *ecdsa.PrivateKey,
	notBefore, notAfter time.Time,
) error {
	var err error
	b.Console, err = newLeaf(leafOptions{
		CN:          ConsoleCN,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		NotBefore:   notBefore,
		NotAfter:    notAfter,
	}, caCert, caKey)
	if err != nil {
		return fmt.Errorf("certs: console client leaf: %w", err)
	}

	b.SignerClient, err = newLeaf(leafOptions{
		CN:          SignerClientCN,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		NotBefore:   notBefore,
		NotAfter:    notAfter,
	}, signerCACert, signerCAKey)
	if err != nil {
		return fmt.Errorf("certs: signer client leaf: %w", err)
	}
	return nil
}

// ReissueConsoleLeaves replaces b.Console and b.SignerClient in place, signing
// each with the authority already in b.
//
// Everything else in b is untouched: both roots, the atlantis server leaf and
// the signer server leaf. The caller writes b back and re-registers; nothing in
// the organisation's namespace has to restart, because no pod mounts these two.
//
// now may be the zero time, which means the current time. It is a parameter so
// a test can mint a credential that is already near expiry without waiting.
func ReissueConsoleLeaves(b *Bundle, now time.Time) error {
	if b == nil {
		return fmt.Errorf("certs: no bundle to reissue from")
	}
	if now.IsZero() {
		now = time.Now()
	}

	// Parsed before anything is minted, so a bundle that cannot sign leaves is
	// reported without having half-replaced the pair. A Console reissued
	// alongside a SignerClient that failed would leave the console able to
	// reach atlantis and not the signer — working until the next enrolment.
	caCert, caKey, err := parseAuthority(b.CA, "issuing CA")
	if err != nil {
		return err
	}
	signerCACert, signerCAKey, err := parseAuthority(b.SignerCA, "signer client CA")
	if err != nil {
		return err
	}

	// The same backdating Generate applies, for the same reason: whatever picks
	// these up runs on a different machine, whose clock is not this one to the
	// second.
	notBefore := now.Add(-5 * time.Minute)
	return mintConsoleLeaves(b, caCert, caKey, signerCACert, signerCAKey,
		notBefore, now.Add(ClientLifetime))
}

// parseAuthority recovers the signing material from a stored Authority.
//
// tls.X509KeyPair rather than parsing the two halves separately, because it
// also proves they belong together — and it reads both PKCS#8 (what encodeKey
// writes) and SEC1 (what `openssl ecparam -genkey` writes, which is what an
// authority created by init-certs.sh carries).
func parseAuthority(a Authority, what string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	if len(a.CertPEM) == 0 || len(a.KeyPEM) == 0 {
		return nil, nil, fmt.Errorf("certs: %s is missing its certificate or key", what)
	}
	pair, err := tls.X509KeyPair(a.CertPEM, a.KeyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("certs: %s: certificate and key are not a pair: %w", what, err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, fmt.Errorf("certs: %s: %w", what, err)
	}
	// Checked rather than assumed. Signing a leaf with a non-CA certificate
	// succeeds here and fails at every handshake afterwards, which is a long way
	// from the thing that caused it.
	if !cert.IsCA {
		return nil, nil, fmt.Errorf("certs: %s is not a certificate authority", what)
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("certs: %s has a %T key, want ECDSA", what, pair.PrivateKey)
	}
	return cert, key, nil
}
