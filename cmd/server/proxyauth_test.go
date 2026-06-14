package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"testing"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// --- test PKI helpers ---

type testCA struct {
	cert *x509.Certificate
	key  crypto.Signer
	pool *x509.CertPool
}

var serialCounter int64

func nextSerial() *big.Int {
	serialCounter++
	return big.NewInt(serialCounter)
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          nextSerial(),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

// issue mints a leaf signed by the CA. eku/notBefore/notAfter let each test
// shape the negative cases (server-only EKU, expired window, etc.).
func (ca *testCA) issue(t *testing.T, cn string, eku []x509.ExtKeyUsage, notBefore, notAfter time.Time) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: nextSerial(),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  eku,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, key.Public(), ca.key)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert
}

// clientCert is the common-case helper: a valid, in-window clientAuth leaf.
func (ca *testCA) clientCert(t *testing.T, cn string) *x509.Certificate {
	return ca.issue(t, cn, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
}

func certPEM(c *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
}

func urlEncodedPEM(c *x509.Certificate) string { return url.PathEscape(certPEM(c)) }

func xfccHeader(c *x509.Certificate) string {
	return fmt.Sprintf(`Hash=abc123;Cert="%s";Subject="CN=%s"`, urlEncodedPEM(c), c.Subject.CommonName)
}

// ctxWith builds a request context whose live TLS peer is peerCert and whose
// metadata carries the given forwarded-cert header values.
func ctxWith(peerCert *x509.Certificate, header string, headerVals ...string) context.Context {
	ctx := context.Background()
	if peerCert != nil {
		ctx = peer.NewContext(ctx, &peer.Peer{
			AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{peerCert},
			}},
		})
	}
	if len(headerVals) > 0 {
		ctx = metadata.NewIncomingContext(ctx, metadata.MD{header: headerVals})
	}
	return ctx
}

// --- tests ---

func TestForwardedAuth_Resolve(t *testing.T) {
	ca := newTestCA(t)
	other := newTestCA(t)
	const hdr = "x-forwarded-client-cert"
	fa := &forwardedAuth{
		proxies:   map[string]struct{}{"atlantis-proxy": {}},
		header:    hdr,
		clientCAs: ca.pool,
	}
	proxyCert := ca.clientCert(t, "atlantis-proxy")

	cases := []struct {
		name        string
		ctx         context.Context
		wantCaller  string
		wantForward bool
		wantErr     bool
	}{
		{
			name:        "happy path: trusted proxy forwards a valid client cert",
			ctx:         ctxWith(proxyCert, hdr, urlEncodedPEM(ca.clientCert(t, "vendor"))),
			wantCaller:  "vendor",
			wantForward: true,
		},
		{
			name:        "XFCC format parses to the same identity",
			ctx:         ctxWith(proxyCert, hdr, xfccHeader(ca.clientCert(t, "vendor"))),
			wantCaller:  "vendor",
			wantForward: true,
		},
		{
			name: "spoofing: a direct (non-proxy) caller forging the header is ignored",
			// peer is a real client cert, NOT a trusted proxy; the forged
			// header must NOT be honored — forwarded=false so the caller
			// stays its own peer identity.
			ctx:         ctxWith(ca.clientCert(t, "vendor"), hdr, urlEncodedPEM(ca.clientCert(t, "catalog"))),
			wantForward: false,
		},
		{
			name: "EKU pin: a forwarded server cert (no clientAuth) is rejected",
			ctx: ctxWith(proxyCert, hdr, urlEncodedPEM(
				ca.issue(t, "vendor", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))),
			wantErr: true,
		},
		{
			name: "expiry: a forwarded expired cert is rejected",
			ctx: ctxWith(proxyCert, hdr, urlEncodedPEM(
				ca.issue(t, "vendor", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)))),
			wantErr: true,
		},
		{
			name:    "wrong CA: a forwarded cert from another CA is rejected",
			ctx:     ctxWith(proxyCert, hdr, urlEncodedPEM(other.clientCert(t, "vendor"))),
			wantErr: true,
		},
		{
			name:    "empty CN forwarded cert is rejected",
			ctx:     ctxWith(proxyCert, hdr, urlEncodedPEM(ca.clientCert(t, ""))),
			wantErr: true,
		},
		{
			name:    "forwarded CN that is a trusted-proxy CN is rejected",
			ctx:     ctxWith(proxyCert, hdr, urlEncodedPEM(ca.clientCert(t, "atlantis-proxy"))),
			wantErr: true,
		},
		{
			name:    "trusted proxy forwards no header is rejected",
			ctx:     ctxWith(proxyCert, hdr),
			wantErr: true,
		},
		{
			name:    "trusted proxy forwards multiple header values is rejected",
			ctx:     ctxWith(proxyCert, hdr, urlEncodedPEM(ca.clientCert(t, "vendor")), urlEncodedPEM(ca.clientCert(t, "catalog"))),
			wantErr: true,
		},
		{
			name: "multi-Cert XFCC (multi-hop) is rejected",
			ctx: ctxWith(proxyCert, hdr, fmt.Sprintf(`Cert="%s",Cert="%s"`,
				urlEncodedPEM(ca.clientCert(t, "vendor")), urlEncodedPEM(ca.clientCert(t, "catalog")))),
			wantErr: true,
		},
		{
			name:        "no peer cert (dev/insecure) → not the proxy path",
			ctx:         ctxWith(nil, hdr, urlEncodedPEM(ca.clientCert(t, "vendor"))),
			wantForward: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caller, der, forwarded, err := fa.resolve(tc.ctx)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got caller=%q forwarded=%v", caller, forwarded)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if forwarded != tc.wantForward {
				t.Fatalf("forwarded=%v, want %v", forwarded, tc.wantForward)
			}
			if forwarded {
				if caller != tc.wantCaller {
					t.Errorf("caller=%q, want %q", caller, tc.wantCaller)
				}
				if len(der) == 0 {
					t.Error("forwarded path must return the cert DER for binding")
				}
			}
		})
	}
}

// TestForwardedAuth_Disabled confirms a nil resolver (mode off) never claims
// the request.
func TestForwardedAuth_Disabled(t *testing.T) {
	var fa *forwardedAuth
	caller, der, forwarded, err := fa.resolve(context.Background())
	if caller != "" || der != nil || forwarded || err != nil {
		t.Fatalf("nil forwardedAuth should be inert, got %q %v %v %v", caller, der, forwarded, err)
	}
}
