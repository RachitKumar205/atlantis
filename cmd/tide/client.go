package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"

	"github.com/rachitkumar205/atlantis/clients/go/adminjson"
	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// adminClient wraps the generated AdminServiceClient with the connection it
// owns, so callers get one thing to defer Close on.
//
// The messages are generated from atlantis/admin/v1/admin.proto, so a field the
// server adds is a field this client has. tidectl, the console and the server
// read the same file.
type adminClient struct {
	adminpb.AdminServiceClient
	conn *grpc.ClientConn
}

// dial opens the mTLS channel to atlantis.
//
// TLS material is required. The server presents a certificate and demands one
// back on every connection, so a client with none gets no channel rather than a
// degraded one. An insecure fallback here would warn and then fail at the
// handshake, naming neither the cause nor the fix.
func dial(cfg *tideConfig) (*adminClient, error) {
	if cfg.TLS.CertPEM == "" {
		return nil, fmt.Errorf(
			"this machine has no certificate for %q, and atlantis requires one.\n\n"+
				"Run `tide login` and approve in the browser.",
			cfg.Caller)
	}
	creds, err := buildTLS(cfg)
	if err != nil {
		return nil, err
	}
	// No ForceCodecV2: the default proto codec applies. That option set the
	// content-subtype connection-wide, which is why the JSON and protobuf
	// paths could never be mixed per-RPC and a client had to move all at once.
	conn, err := grpc.NewClient(cfg.Endpoint,
		grpc.WithTransportCredentials(creds),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.Endpoint, err)
	}
	return &adminClient{AdminServiceClient: adminpb.NewAdminServiceClient(conn), conn: conn}, nil
}

func (c *adminClient) Close() error { return c.conn.Close() }

// emitJSON writes a response to stdout in the admin JSON dialect.
//
// Every --format=json path goes through here rather than through
// encoding/json, so tide and tidectl cannot drift apart on what their output
// looks like. See clients/go/adminjson for what the dialect is and why 64-bit
// integers are quoted.
//
// inlineJSONBytes names the `bytes` fields on this response that hold a JSON
// document rather than opaque octets. They would otherwise render as base64,
// which is correct by the spec and useless in a pipeline.
func emitJSON(m proto.Message, inlineJSONBytes ...string) error {
	b, err := adminjson.MarshalIndentInlining(m, inlineJSONBytes...)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, string(b))
	return err
}

func buildTLS(cfg *tideConfig) (credentials.TransportCredentials, error) {
	// One source: the credential store, loaded by applyStoreCredentials. See
	// tideConfig for why nothing else can supply this material.
	//
	// CertPEM and KeyPEM hold the same bytes: one file with the key and the
	// certificate in it, so renewal is a single atomic rename. X509KeyPair
	// searches each buffer independently and finds the half it needs in both.
	cert, err := tls.X509KeyPair([]byte(cfg.TLS.CertPEM), []byte(cfg.TLS.KeyPEM))
	if err != nil {
		return nil, fmt.Errorf("the stored certificate for %q does not load: %w", cfg.Caller, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cfg.TLS.CAPEM)) {
		return nil, fmt.Errorf("the stored CA for %q holds no certificate", cfg.Caller)
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}), nil
}
