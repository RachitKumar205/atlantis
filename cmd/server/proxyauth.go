package main

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/url"
	"strings"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	"github.com/rachitkumar205/atlantis/internal/server/interceptors"
)

// proxyauth.go implements trusted front-proxy identity: when a reverse proxy
// (nginx/caddy/envoy) terminates the client's mTLS at the edge, it forwards
// the verified end-client cert in a header. This server re-validates that
// cert against its OWN client-CA pool and derives caller identity (and the
// cert-binding fingerprint) from it — the proxy delegates only the TLS
// transport; every identity/authz decision stays cryptographically here.
//
// The whole security of the mode is the re-validation in (*forwardedAuth).
// verify: chain-to-CA AND clientAuth key usage AND validity window. The EKU
// pin rejects any leaf that carries a non-clientAuth extended key usage (a
// serverAuth-only cert, say); a leaf with no EKU is valid for any usage and
// passes, so it is the cert-binding registration check that ultimately
// keeps a non-caller cert from being asserted. Without the validity check,
// an expired cert would authenticate indefinitely — the proxy now owns the
// TLS-handshake expiry check that used to be the only one.

// maxForwardedCertHeaderBytes caps the forwarded-cert header before any
// parsing/verification work, so a flood of junk headers can't amplify into
// CPU. A url-encoded leaf PEM is ~2-3 KB; 16 KB is generous headroom.
const maxForwardedCertHeaderBytes = 16 << 10

// forwardedAuth resolves a trusted-proxy-forwarded client identity. Nil when
// the mode is disabled (no ATL_TRUSTED_PROXY_CALLERS configured).
type forwardedAuth struct {
	proxies   map[string]struct{} // trusted-proxy CNs (live peer must be one of these)
	header    string              // lowercased metadata key carrying the forwarded cert
	clientCAs *x509.CertPool      // same trust root as the listener's ClientCAs
}

// newForwardedAuth builds the resolver from config, or returns nil when no
// trusted proxies are configured (the mode stays inert). Requires TLS — the
// caller (loadConfig) already enforces that ATL_TRUSTED_PROXY_CALLERS
// implies mTLS.
func newForwardedAuth(cfg config) (*forwardedAuth, error) {
	if len(cfg.TrustedProxyCallers) == 0 {
		return nil, nil
	}
	pool, err := loadClientCAPool(cfg.TLSCAFile)
	if err != nil {
		return nil, err
	}
	proxies := make(map[string]struct{}, len(cfg.TrustedProxyCallers))
	for _, p := range cfg.TrustedProxyCallers {
		proxies[p] = struct{}{}
	}
	header := strings.ToLower(strings.TrimSpace(cfg.TrustedProxyCertHeader))
	if header == "" {
		header = "x-forwarded-client-cert"
	}
	return &forwardedAuth{proxies: proxies, header: header, clientCAs: pool}, nil
}

// resolve returns the forwarded caller identity when the live peer cert is a
// trusted proxy. forwarded reports whether the trusted-proxy path applied;
// when false the request takes the normal peer-cert identity path. A non-nil
// error means the live peer IS a trusted proxy but forwarded no/invalid
// identity — the request must be rejected (Unauthenticated).
//
// SECURITY: the trusted-proxy decision reads the LIVE peer cert CN only,
// never a header. A direct client forging the header has a non-proxy peer
// CN, so the header is never honored for it.
func (fa *forwardedAuth) resolve(ctx context.Context) (caller string, certDER []byte, forwarded bool, err error) {
	if fa == nil {
		return "", nil, false, nil
	}
	proxyCN, ok := livePeerCN(ctx)
	if !ok {
		return "", nil, false, nil // no peer cert (dev/insecure) → not the proxy path
	}
	if _, isProxy := fa.proxies[proxyCN]; !isProxy {
		return "", nil, false, nil // direct client → normal peer-cert path
	}

	// The live peer is a trusted proxy: a CN in ATL_TRUSTED_PROXY_CALLERS is
	// ONLY ever a proxy, so it must forward a valid end-client identity.
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get(fa.header)
	if len(vals) == 0 || strings.TrimSpace(vals[0]) == "" {
		return "", nil, false, fmt.Errorf("trusted proxy %q forwarded no client certificate", proxyCN)
	}
	if len(vals) > 1 {
		return "", nil, false, fmt.Errorf("trusted proxy %q forwarded multiple client certificates", proxyCN)
	}
	cert, perr := parseForwardedCert(vals[0])
	if perr != nil {
		return "", nil, false, fmt.Errorf("trusted proxy %q forwarded an unparseable client certificate: %w", proxyCN, perr)
	}
	if verr := fa.verify(cert); verr != nil {
		return "", nil, false, fmt.Errorf("trusted proxy %q forwarded an invalid client certificate: %w", proxyCN, verr)
	}
	cn := cert.Subject.CommonName
	if cn == "" {
		return "", nil, false, fmt.Errorf("forwarded client certificate has empty CN")
	}
	if _, clash := fa.proxies[cn]; clash {
		// A forwarded leaf must never assert a trusted-proxy identity.
		return "", nil, false, fmt.Errorf("forwarded client certificate CN %q is a trusted-proxy CN", cn)
	}
	return cn, cert.Raw, true, nil
}

// verify replicates Go's RequireAndVerifyClientCert checks for the forwarded
// cert: chain-to-CA + clientAuth EKU + validity window (CurrentTime defaults
// to now). This is the security boundary of the trusted-proxy mode.
func (fa *forwardedAuth) verify(cert *x509.Certificate) error {
	_, err := cert.Verify(x509.VerifyOptions{
		Roots:     fa.clientCAs,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	return err
}

// proxyForwardedFromContext returns the predicate the admin service uses to
// tell whether a request's identity was asserted by a trusted front proxy.
// Returns nil when the mode is off (fa == nil), which disables the admin-
// plane gating entirely.
func proxyForwardedFromContext(fa *forwardedAuth) func(context.Context) bool {
	if fa == nil {
		return nil
	}
	return func(ctx context.Context) bool {
		_, ok := interceptors.ForwardedCertFromContext(ctx)
		return ok
	}
}

// livePeerCN extracts the CN of the live TLS peer's leaf cert (the proxy, in
// trusted-proxy mode). Returns false in non-TLS modes.
func livePeerCN(ctx context.Context) (string, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", false
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", false
	}
	if len(tlsInfo.State.PeerCertificates) == 0 {
		return "", false
	}
	return tlsInfo.State.PeerCertificates[0].Subject.CommonName, true
}

// parseForwardedCert extracts a single end-client cert from a forwarded
// header value, accepting both a bare URL-encoded PEM (nginx
// $ssl_client_escaped_cert / Caddy) and an Envoy XFCC value with a single
// Cert= element. Multiple certs (a chain or multi-hop) are rejected.
func parseForwardedCert(headerVal string) (*x509.Certificate, error) {
	v := strings.TrimSpace(headerVal)
	if v == "" {
		return nil, fmt.Errorf("empty forwarded cert")
	}
	if len(v) > maxForwardedCertHeaderBytes {
		return nil, fmt.Errorf("forwarded cert header exceeds %d bytes", maxForwardedCertHeaderBytes)
	}

	blob := v
	// Envoy XFCC: "Hash=..;Cert=\"<url-enc PEM>\";Subject=..". A bare PEM is
	// percent-encoded and never contains the literal token "Cert=".
	if i := strings.Index(v, "Cert="); i >= 0 {
		if strings.Count(v, "Cert=") > 1 {
			return nil, fmt.Errorf("multiple forwarded certs (multi-hop not allowed)")
		}
		blob = xfccCertValue(v[i+len("Cert="):])
	}

	dec, err := url.PathUnescape(blob)
	if err != nil {
		// Not percent-encoded; fall back to the raw blob (some proxies send
		// unescaped base64 DER).
		dec = blob
	}
	der, err := decodeCertBlob(dec)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// xfccCertValue pulls the Cert element value out of the remainder of an XFCC
// header after "Cert=". The value is either double-quoted or runs to the
// next ';' or ','.
func xfccCertValue(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, `"`) {
		s = s[1:]
		if j := strings.Index(s, `"`); j >= 0 {
			return s[:j]
		}
		return s
	}
	if j := strings.IndexAny(s, ";,"); j >= 0 {
		return strings.TrimSpace(s[:j])
	}
	return s
}

// decodeCertBlob turns a forwarded cert blob into DER, accepting PEM (the
// common case) or raw base64-DER (Caddy's der_base64 / some LBs). It does
// NOT trust the bytes — the caller re-verifies the parsed cert.
func decodeCertBlob(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if block, _ := pem.Decode([]byte(s)); block != nil && block.Type == "CERTIFICATE" {
		return block.Bytes, nil
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if der, derr := enc.DecodeString(s); derr == nil && len(der) > 0 {
			if _, perr := x509.ParseCertificate(der); perr == nil {
				return der, nil
			}
		}
	}
	return nil, fmt.Errorf("forwarded cert is neither PEM nor base64 DER")
}
