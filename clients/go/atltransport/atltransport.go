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
// # Why there is no insecure mode
//
// [Credentials] used to return insecure.NewCredentials when ATL_TLS_CERT was
// unset, and this doc called that "the correct choice for dev / same-cluster
// prod where atlantis is reachable on a private bridge". Both halves were
// wrong once the server stopped accepting plaintext:
//
//   - The server requires a client certificate on every connection. A caller
//     without one does not get a private-bridge channel; it gets a handshake
//     failure, one layer below where the cause is legible.
//   - The certificate is not only transport security. atlantis identifies the
//     caller by its certificate CN, and the caller allowlist, the
//     caller-to-cert binding and the capability grants all key off it. A
//     connection with no client cert has no identity, so there is nothing for
//     authorization to be about.
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

// Dial returns a gRPC client connection to addr with credentials sourced
// from Credentials() plus any extra opts the caller provides. Use this
// for everything except where you need custom call-option machinery
// (e.g. the JSON codec for admin RPCs — see the WithDialOption escape
// hatch in those call sites).
func Dial(addr string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	creds, err := Credentials()
	if err != nil {
		return nil, err
	}
	// gRPC defaults the client receive limit to 4 MiB, which is too small
	// for bulk entity Query reads — a single page of rows carrying large
	// jsonb/blob columns (e.g. large raw_data payloads) overflows it with
	// "received message larger than max". Raise the default; callers can
	// still override by passing their own WithDefaultCallOptions in opts
	// (the later value wins).
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
