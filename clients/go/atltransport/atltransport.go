// Package atltransport builds gRPC transport credentials and connections
// for the atlantis backend. Centralising this here means every caller
// (the typed Go SDK, custom dialers like the job submitter, tide / tidectl)
// reads the same env-var contract and uses the same TLS material.
//
// Env-var contract — all three are required:
//
//	ATL_TLS_CERT  path to the caller's mTLS client cert (PEM)
//	ATL_TLS_KEY   path to the caller's mTLS private key (PEM)
//	ATL_TLS_CA    path to the atlantis server's CA bundle (PEM)
//
// There is no insecure mode. Falling back to insecure.NewCredentials when
// ATL_TLS_CERT is unset gives a caller a handshake failure rather than a
// plaintext channel, one layer below where the cause is legible.
//
// The certificate is also the identity: atlantis reads the caller from its CN,
// and the allowlist, the caller-to-cert binding and the capability grants all
// key off that. A connection with no client certificate has nothing for
// authorization to be about.
package atltransport

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Credentials returns gRPC transport credentials for the atlantis client.
// See the package doc for the env-var contract. MinVersion=TLS13 matches
// the atlantis server's listener so any downgrade attempt fails cleanly.
func Credentials() (credentials.TransportCredentials, error) {
	certPath := os.Getenv("ATL_TLS_CERT")
	keyPath := os.Getenv("ATL_TLS_KEY")
	caPath := os.Getenv("ATL_TLS_CA")

	var missing []string
	for _, v := range []struct{ name, val string }{
		{"ATL_TLS_CERT", certPath},
		{"ATL_TLS_KEY", keyPath},
		{"ATL_TLS_CA", caPath},
	} {
		if v.val == "" {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf(
			"atltransport: %s not set. atlantis authenticates every caller by "+
				"client certificate and accepts no plaintext connection, so all "+
				"three of ATL_TLS_CERT, ATL_TLS_KEY and ATL_TLS_CA are required",
			strings.Join(missing, ", "))
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("atltransport: load client cert: %w", err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("atltransport: read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("atltransport: no usable certs in CA file %s", caPath)
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}), nil
}

// Dial returns a gRPC client connection to addr, with credentials from
// Credentials and any extra opts. A call site needing its own codec — the admin
// RPCs' JSON one — passes it through opts.
func Dial(addr string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	creds, err := Credentials()
	if err != nil {
		return nil, err
	}
	// gRPC's 4 MiB client receive limit is too small for a bulk entity Query: one
	// page of rows carrying large jsonb or blob columns overflows it with
	// "received message larger than max". A caller's own WithDefaultCallOptions
	// in opts overrides this, since the later value wins.
	defaults := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxRecvMsgBytes)),
	}
	opts = append(defaults, opts...)
	return grpc.NewClient(addr, opts...)
}

// maxRecvMsgBytes is the default client-side max receive size (64 MiB).
// Generous headroom for bulk reads without being unbounded.
const maxRecvMsgBytes = 64 << 20
