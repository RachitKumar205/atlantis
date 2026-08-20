// Package testpki mints the mTLS material a test needs to start atlantis.
//
// atlantis accepts no plaintext connection: the listener requires a client
// certificate, and the caller allowlist, the caller-to-cert binding and the
// capability grants are all keyed to the CN it carries. So a test that boots
// the server, or that stands up an in-process admin service for the console to
// dial, needs a CA and leaf certificates before it can do anything at all.
//
// # Why generated rather than committed
//
// A checked-in certificate expires, and the failure lands years later on
// whoever is unlucky, reading as a broken test rather than an expired fixture.
// A checked-in CA key is a private key in a public repository, and this one
// signs certificates the server trusts. Generating per test costs a few
// milliseconds of P-256 and has neither problem.
//
// # Why one package rather than a helper per caller
//
// Two packages needed it within an hour of each other — cmd/server for its boot
// and reload children, internal/console for the admin service its harness
// dials. A third will want it. The copies would drift in the way that matters
// here: one grows a SAN or an EKU the others lack, and the difference shows up
// as a handshake failure in whichever test was not updated.
//
// Not usable from clients/go: that is a separate module, and internal/ is
// unreachable from outside the module that declares it.
package testpki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// PKI is a certificate authority and the server leaf it signed, on disk.
type PKI struct {
	// Dir holds every file this PKI has written.
	Dir string

	// CAFile, CertFile and KeyFile are what the server's TLS_CA_FILE,
	// TLS_CERT_FILE and TLS_KEY_FILE want.
	CAFile   string
	CertFile string
	KeyFile  string

	// CAKeyFile is the authority's own private key, beside CAFile.
	//
	// Written because cmd/signer loads a CA from a directory holding both —
	// it issues certificates, so it needs the key, unlike every other consumer
	// here which only verifies against the certificate. A test that wanted to
	// boot the signer otherwise had to build a second, near-identical CA of its
	// own.
	//
	// It is a private key on disk under t.TempDir(), which is the same posture
	// as every other key this package writes.
	CAKeyFile string

	caCert *x509.Certificate
	caKey  crypto.Signer
}

// serial counts up so no two certificates in one process collide. Atomic
// because tests run in parallel and a duplicate serial from the same issuer is
// malformed.
var serial atomic.Int64

func nextSerial() *big.Int { return big.NewInt(serial.Add(1)) }

// New writes a CA and a server certificate into dir.
//
// The server leaf names localhost, atlantis and 127.0.0.1, which covers both
// the loopback address a test server binds and the hostname a compose-style
// client would use.
func New(t *testing.T, dir string) *PKI {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("testpki: ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          nextSerial(),
		Subject:               pkix.Name{CommonName: "atlantis-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caKey.Public(), caKey)
	if err != nil {
		t.Fatalf("testpki: ca cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("testpki: parse ca: %v", err)
	}

	p := &PKI{
		Dir:       dir,
		CAFile:    filepath.Join(dir, "ca.crt"),
		CAKeyFile: filepath.Join(dir, "ca.key"),
		CertFile:  filepath.Join(dir, "server.crt"),
		KeyFile:   filepath.Join(dir, "server.key"),
		caCert:    caCert,
		caKey:     caKey,
	}
	writePEM(t, p.CAFile, "CERTIFICATE", caDER)

	caKeyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		t.Fatalf("testpki: marshal ca key: %v", err)
	}
	writePEM(t, p.CAKeyFile, "EC PRIVATE KEY", caKeyDER)

	srvDER, srvKeyDER := p.issue(t, "atlantis",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		[]string{"localhost", "atlantis"},
		[]net.IP{net.ParseIP("127.0.0.1")})
	writePEM(t, p.CertFile, "CERTIFICATE", srvDER)
	writePEM(t, p.KeyFile, "EC PRIVATE KEY", srvKeyDER)

	return p
}

// ClientCert mints a client leaf with the given CN and returns its cert and key
// paths. The CN is the caller identity atlantis authorizes against.
func (p *PKI) ClientCert(t *testing.T, cn string) (certFile, keyFile string) {
	t.Helper()
	der, keyDER := p.issue(t, cn,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil, nil)

	certFile = filepath.Join(p.Dir, cn+".crt")
	keyFile = filepath.Join(p.Dir, cn+".key")
	writePEM(t, certFile, "CERTIFICATE", der)
	writePEM(t, keyFile, "EC PRIVATE KEY", keyDER)
	return certFile, keyFile
}

// ExpiredClientCert mints a client leaf that expired an hour ago.
//
// For tests about what happens when a certificate is past its validity, which
// is otherwise only reachable by waiting. The files are named apart from
// ClientCert's so both can be minted from one PKI — ClientCert derives its
// filenames from the CN, so two leaves with one CN would overwrite each other.
func (p *PKI) ExpiredClientCert(t *testing.T, cn string) (certFile, keyFile string) {
	t.Helper()
	der, keyDER := p.issueUntil(t, cn,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil, nil,
		time.Now().Add(-time.Hour))

	certFile = filepath.Join(p.Dir, cn+".expired.crt")
	keyFile = filepath.Join(p.Dir, cn+".expired.key")
	writePEM(t, certFile, "CERTIFICATE", der)
	writePEM(t, keyFile, "EC PRIVATE KEY", keyDER)
	return certFile, keyFile
}

// ServerTLS is a tls.Config for a listener that demands and verifies a client
// certificate from this CA — the same posture cmd/server's transportCreds
// builds, so a test server behaves like the real one.
func (p *PKI) ServerTLS(t *testing.T) *tls.Config {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(p.CertFile, p.KeyFile)
	if err != nil {
		t.Fatalf("testpki: load server keypair: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(p.caCert)
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}
}

// SignCSR issues a client certificate for a certificate signing request,
// returning it PEM-encoded alongside this authority's own certificate.
//
// What a fake signer needs to be a fake of something real. Every other method
// here generates the keypair too, which is exactly what enrolment exists to
// stop: the point of a CSR is that the private key stayed on the machine that
// made it, and a stand-in that quietly minted its own key would let a console
// pass a test it should fail.
//
// The subject is built from cn, not copied from the request — the same as
// cmd/signer, which names the leaf from the caller it was handed and takes only
// the public key from the CSR. A stand-in that copied the CSR's subject would
// let a console that stopped sending the token row's caller still look correct.
//
// # Why this returns an error rather than taking a *testing.T
//
// It is called from an HTTP handler, which runs on the server's goroutine.
// t.Fatalf there calls runtime.Goexit on a goroutine that is not the test's:
// the test is marked failed but the handler never answers, so the client waits
// on a connection nobody will close and the run hangs somewhere unrelated. A
// returned error becomes a 4xx, which is what the code under test should see
// anyway.
func (p *PKI) SignCSR(csrPEM, cn string) (certPEM, caPEM string, err error) {
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return "", "", errors.New("testpki: not a PEM CERTIFICATE REQUEST block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return "", "", fmt.Errorf("testpki: parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return "", "", fmt.Errorf("testpki: CSR signature: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: nextSerial(),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	// csr.PublicKey — the key the requester holds. Nothing here ever sees its
	// private half, which is the whole property under test.
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, csr.PublicKey, p.caKey)
	if err != nil {
		return "", "", fmt.Errorf("testpki: sign CSR: %w", err)
	}

	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	caPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.caCert.Raw}))
	return certPEM, caPEM, nil
}

func (p *PKI) issue(
	t *testing.T, cn string, eku []x509.ExtKeyUsage, dns []string, ips []net.IP,
) (certDER, keyDER []byte) {
	t.Helper()
	return p.issueUntil(t, cn, eku, dns, ips, time.Now().Add(24*time.Hour))
}

// issueUntil is issue with the expiry chosen by the caller, so a test can mint
// a certificate that is already invalid.
func (p *PKI) issueUntil(
	t *testing.T, cn string, eku []x509.ExtKeyUsage, dns []string, ips []net.IP, notAfter time.Time,
) (certDER, keyDER []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("testpki: %s key: %v", cn, err)
	}
	// NotBefore is pinned behind NotAfter rather than to a fixed hour ago, or
	// an expired certificate would also be one that is not yet valid — and a
	// test asserting on "expired" would pass against code that only checks the
	// other end.
	notBefore := time.Now().Add(-time.Hour)
	if !notAfter.After(notBefore) {
		notBefore = notAfter.Add(-time.Hour)
	}
	tmpl := &x509.Certificate{
		SerialNumber: nextSerial(),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  eku,
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	certDER, err = x509.CreateCertificate(rand.Reader, tmpl, p.caCert, key.Public(), p.caKey)
	if err != nil {
		t.Fatalf("testpki: %s cert: %v", cn, err)
	}
	keyDER, err = x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("testpki: marshal %s key: %v", cn, err)
	}
	return certDER, keyDER
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	b := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("testpki: write %s: %v", path, err)
	}
}
