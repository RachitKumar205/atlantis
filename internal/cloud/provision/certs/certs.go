// Package certs mints the TLS material for one organisation's atlantis stack.
//
// This is deploy/init-certs.sh expressed in Go, because provisioning happens in
// a controller and a controller cannot shell out to 402 lines of openssl. What
// it produces is deliberately the same shape, and the reasoning in that script
// is the reasoning here.
//
// # One organisation, one pair of authorities
//
// Every organisation gets its own two roots. That is not tidiness, it is the
// isolation boundary: caller identity at the server is the client certificate's
// common name and nothing else (cmd/server/auth.go, resolveCaller), and caller
// names are chosen by customers. "backend", "api" and "prod" will collide
// across organisations. With one fleet-wide authority, customer A's certificate
// would authenticate at customer B's server and match B's caller row of the
// same name. With a root per organisation the handshake fails first, which is
// the only mechanism here that does not depend on anybody remembering anything.
//
// # Two authorities, not one, and they must never be merged
//
// The distinction is subtle enough that init-certs.sh spends twenty lines on
// it, so it is worth repeating rather than referring to:
//
//	CA        is what caller certificates are issued FROM.
//	SignerCA  is what the signer accepts its clients BY.
//
// Every caller certificate is marked for client authentication. If the signer
// verified incoming clients against the authority it issues from, then every
// certificate it had ever issued would also be a valid credential for talking
// to it — caller "backend" could present its own legitimate certificate, ask
// for one named "payments", and get it. Nothing is signed by both roots, and
// this package has a test whose only job is to keep that true.
//
// # What this does not mint
//
// The enrolment listener's own TLS certificate. That listener belongs to the
// shared console, not to an organisation: one hostname, one certificate, and in
// a deployment it is publicly trusted so that `tide login` verifies it against
// the system roots. What is per-organisation is the *client* CA that listener
// verifies renewing machines against, and that is a pool assembled from the
// CertPEM of every organisation's CA rather than a certificate minted here.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// The common names below are load-bearing values, not labels. Changing one
// silently breaks a check somewhere else in the system:
//
//   - ConsoleCN is the identity migrations/infra/0019 seeds into
//     atlantis.caller_identities, and the default of
//     ATL_CERT_BINDING_EXEMPT_CALLERS. A console presenting any other name is
//     refused by the server's allowlist.
//   - SignerClientCN is matched against SIGNER_ALLOWED_CLIENT_CNS, which the
//     signer refuses to default. It is the same string as ConsoleCN and that is
//     deliberate: it is the console in both cases. The certificates are
//     different because they chain to different roots.
//   - SignerServerCN is what the console expects to be talking to.
//
// They are identical across organisations. The certificate that carries them is
// not, because the root differs — which is the whole design.
const (
	ServerCN       = "atlantis"
	ConsoleCN      = "atlantis-console"
	SignerServerCN = "atlantis-signer"
	SignerClientCN = "atlantis-console"
)

// Lifetimes, matching init-certs.sh rather than improving on it.
//
// ServerLifetime is under Apple's 825-day ceiling for TLS server certificates,
// verified at the boundary: an 825-day self-signed certificate fails with
// "certificate signed by unknown authority" and an 826-day one with
// "certificate is not standards compliant" — a message that names one of
// Apple's rules and says nothing about trust, which is exactly the kind of
// error that costs an afternoon.
//
// The roots stay long because nothing rotates them, and that is not an
// oversight. Replacing an authority invalidates every caller certificate issued
// under it, so the fix for a compromised root is a rebuild with a re-enrolment,
// not a shorter life — ensureCerts refuses to regenerate one for this reason.
//
// # Why ClientLifetime is thirty days and used to be ten years
//
// It was long, with a comment arguing that shortening it "without an online
// rotation path does not make the system safer; it schedules an outage". That
// was correct, and it stopped being true when the provisioner learned to reissue
// these leaves on its reconcile pass — see RotateConsoleCredentials. Thirty days
// is the same reasoning migration 0032 used when it dropped certificate pinning:
// smallstep puts service certificates at "one month or less" and relies on
// expiry rather than revocation, and this is now a certificate that renews
// itself.
//
// The number is not chosen to be as short as possible. Rotation begins at ten
// days remaining, and those ten days are the margin: how long the provisioner
// can be wedged, restarted badly, or simply not deployed before an organisation
// loses console access. A shorter life would spend that margin to reduce a
// window that is already bounded.
//
// This covers only the two certificates the console presents. It is the whole
// of what rotation replaces, and nothing else reads it — so shortening it
// cannot reach a caller's certificate, which the signer issues with a life of
// its own.
//
// # What a short life here does and does not buy
//
// It bounds a credential that leaked once and was not noticed. It does not
// contain a console that is still compromised: that process holds
// CONSOLE_DATA_KEY and can unseal whatever the current credential is, however
// often it changes. Cutting one off deliberately is `cloud org revoke-console`,
// which takes effect in five seconds rather than thirty days.
const (
	CALifetime     = 3650 * 24 * time.Hour
	ServerLifetime = 820 * 24 * time.Hour
	ClientLifetime = 30 * 24 * time.Hour
)

// Authority is a self-signed root and its private key.
//
// KeyPEM is the material that must never reach an atlantis pod. Only the signer
// mounts it, and only for CA. Callers of this package are responsible for that
// separation; nothing here can enforce it.
type Authority struct {
	CertPEM []byte
	KeyPEM  []byte
}

// Leaf is an end-entity certificate and its private key.
type Leaf struct {
	CertPEM []byte
	KeyPEM  []byte
}

// Bundle is everything one organisation's stack needs.
type Bundle struct {
	// CA issues caller certificates and signs the two leaves below it. Its
	// CertPEM is what the organisation's atlantis trusts (TLS_CA_FILE), what
	// the console verifies the organisation against, and what the shared
	// enrolment listener adds to its client-CA pool.
	CA     Authority
	Server Leaf // atlantis's TLS server identity
	// Console is the client certificate the console presents TO atlantis. It
	// chains to CA, so atlantis accepts it the same way it accepts a caller.
	Console Leaf

	// SignerCA is the independent second root. Nothing it signs is signed by
	// CA, and nothing CA signs is signed by it.
	SignerCA     Authority
	SignerServer Leaf // the signer's TLS server identity
	// SignerClient is the client certificate the console presents TO the
	// signer. Same common name as Console, different root — see the package
	// comment.
	SignerClient Leaf
}

// Options describes one organisation's stack.
type Options struct {
	// Org names the organisation. It appears only in the roots' common names,
	// for the benefit of whoever is reading a handshake, and is never an
	// identity — see the common-name constants.
	Org string

	// ServerDNSNames and ServerIPs go in the atlantis leaf's subject
	// alternative names.
	//
	// Both addresses an organisation is reached by belong here, and getting
	// this wrong is a provisioning bug rather than a configuration one:
	// internal/console/client.go leaves tls.Config.ServerName unset on purpose,
	// so the leaf has to match whatever address the caller dialled. There are
	// two of those — the one the console dials (in-cluster) and the one a
	// caller dials (public) — and console.orgs stores them separately for
	// exactly this reason.
	ServerDNSNames []string
	ServerIPs      []net.IP

	// SignerDNSNames and SignerIPs do the same for the signer's leaf.
	SignerDNSNames []string
	SignerIPs      []net.IP

	// Now fixes the validity window. Zero means time.Now(). Tests set it.
	Now time.Time
}

// Generate mints a complete bundle.
//
// Every call produces fresh keys. There is no "reuse the existing root if one
// is present" path here on purpose: this package does not know what already
// exists, and a function that sometimes reuses and sometimes regenerates is one
// that silently orphans every certificate under a root it decided to replace.
// Deciding that is the provisioner's job, which has the state to do it.
func Generate(opts Options) (*Bundle, error) {
	if opts.Org == "" {
		return nil, fmt.Errorf("certs: Org is required")
	}
	// Both server leaves are checked, not just atlantis's. A leaf with no
	// subject alternative name verifies against nothing, and the failure
	// arrives as a confusing handshake error a long way from here rather than
	// as "you forgot the SAN".
	//
	// The signer is easy to forget because it feels internal, but the console
	// reaches it over an https:// URL (ATL_SIGNER_ADDR) and Go's client checks
	// the hostname like any other. A signer leaf with no SAN is a certificate
	// that cannot work, minted successfully.
	if len(opts.ServerDNSNames) == 0 && len(opts.ServerIPs) == 0 {
		return nil, fmt.Errorf("certs: the atlantis leaf needs at least one DNS name or IP")
	}
	if len(opts.SignerDNSNames) == 0 && len(opts.SignerIPs) == 0 {
		return nil, fmt.Errorf("certs: the signer leaf needs at least one DNS name or IP")
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	// Backdate slightly. Provisioning writes these certificates and something
	// else immediately uses them, on a different machine whose clock is not
	// the same to the second.
	notBefore := now.Add(-5 * time.Minute)

	b := &Bundle{}

	ca, caCert, caKey, err := newAuthority(
		fmt.Sprintf("atlantis-%s-ca", opts.Org), notBefore, now.Add(CALifetime))
	if err != nil {
		return nil, fmt.Errorf("certs: issuing CA: %w", err)
	}
	b.CA = ca

	signerCA, signerCACert, signerCAKey, err := newAuthority(
		fmt.Sprintf("atlantis-%s-signer-clients", opts.Org), notBefore, now.Add(CALifetime))
	if err != nil {
		return nil, fmt.Errorf("certs: signer client CA: %w", err)
	}
	b.SignerCA = signerCA

	b.Server, err = newLeaf(leafOptions{
		CN:          ServerCN,
		DNSNames:    opts.ServerDNSNames,
		IPs:         opts.ServerIPs,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		NotBefore:   notBefore,
		NotAfter:    now.Add(ServerLifetime),
	}, caCert, caKey)
	if err != nil {
		return nil, fmt.Errorf("certs: atlantis server leaf: %w", err)
	}

	b.SignerServer, err = newLeaf(leafOptions{
		CN:          SignerServerCN,
		DNSNames:    opts.SignerDNSNames,
		IPs:         opts.SignerIPs,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		NotBefore:   notBefore,
		NotAfter:    now.Add(ServerLifetime),
	}, signerCACert, signerCAKey)
	if err != nil {
		return nil, fmt.Errorf("certs: signer server leaf: %w", err)
	}

	// The console's two leaves, minted by the same function a rotation uses so
	// that a certificate this produces and one ReissueConsoleLeaves produces
	// cannot differ. See rotate.go.
	if err := mintConsoleLeaves(b, caCert, caKey, signerCACert, signerCAKey,
		notBefore, now.Add(ClientLifetime)); err != nil {
		return nil, err
	}

	return b, nil
}

func newAuthority(cn string, notBefore, notAfter time.Time) (Authority, *x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Authority{}, nil, nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return Authority{}, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		// MaxPathLenZero: this root signs end-entity certificates and nothing
		// else. An intermediate under it would be a second way to mint a
		// caller identity, which is the thing the whole design is narrowing.
		MaxPathLen:     0,
		MaxPathLenZero: true,
		KeyUsage:       x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return Authority{}, nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return Authority{}, nil, nil, err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return Authority{}, nil, nil, err
	}
	return Authority{CertPEM: encodeCert(der), KeyPEM: keyPEM}, cert, key, nil
}

type leafOptions struct {
	CN          string
	DNSNames    []string
	IPs         []net.IP
	ExtKeyUsage []x509.ExtKeyUsage
	NotBefore   time.Time
	NotAfter    time.Time
}

func newLeaf(o leafOptions, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (Leaf, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Leaf{}, err
	}
	serial, err := newSerial()
	if err != nil {
		return Leaf{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: o.CN},
		NotBefore:    o.NotBefore,
		NotAfter:     o.NotAfter,
		// KeyUsageDigitalSignature covers both ECDHE server certificates and
		// client certificates. KeyEncipherment is deliberately absent: it is an
		// RSA key-transport usage and means nothing for an EC key.
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           o.ExtKeyUsage,
		BasicConstraintsValid: true,
		DNSNames:              o.DNSNames,
		IPAddresses:           o.IPs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		return Leaf{}, err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return Leaf{}, err
	}
	return Leaf{CertPEM: encodeCert(der), KeyPEM: keyPEM}, nil
}

// newSerial draws a 128-bit random serial.
//
// Sequential serials would need state that survives across provisioning runs,
// and there is nowhere sensible to keep it. Random at this width is what
// public CAs do and the collision probability is not worth a sentence.
func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func encodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// encodeKey writes PKCS#8. init-certs.sh produces SEC1 ("EC PRIVATE KEY")
// because that is what `openssl ecparam -genkey` emits; tls.X509KeyPair reads
// either, and PKCS#8 is the one that does not need a different block type per
// algorithm.
func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
