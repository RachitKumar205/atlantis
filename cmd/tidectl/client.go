package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"google.golang.org/protobuf/proto"

	"github.com/rachitkumar205/atlantis/clients/go/adminjson"
	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// adminClient wraps the generated AdminServiceClient with the connection it
// owns, so callers get one thing to defer Close on.
//
// The request and response types are the ones generated from
// atlantis/admin/v1/admin.proto and published in the clients/go module. Before
// that proto existed, this file carried a hand-rolled JSON-envelope codec and
// every subcommand redeclared the message it sent — the same shapes tide, the
// console, and the server each maintained their own copy of. A field added to
// one and forgotten in another was a silent wire mismatch that compiled.
type adminClient struct {
	adminpb.AdminServiceClient
	conn *grpc.ClientConn
}

type adminDialConfig struct {
	Endpoint string
	TLSCert  string
	TLSKey   string
	TLSCA    string
}

func dialAdmin(cfg adminDialConfig) (*adminClient, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("endpoint is required")
	}
	var creds credentials.TransportCredentials
	if cfg.TLSCert != "" {
		c, err := buildTLS(cfg)
		if err != nil {
			return nil, err
		}
		creds = c
	} else {
		fmt.Fprintln(os.Stderr, "tidectl: TLS not configured — using insecure transport (dev only)")
		creds = insecure.NewCredentials()
	}
	// No ForceCodecV2: the default proto codec applies. That option set the
	// content-subtype for the whole connection, which is why the JSON and
	// protobuf paths could never be mixed per-RPC and the cutover had to move
	// a whole client at once.
	conn, err := grpc.NewClient(cfg.Endpoint,
		grpc.WithTransportCredentials(creds),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.Endpoint, err)
	}
	return &adminClient{AdminServiceClient: adminpb.NewAdminServiceClient(conn), conn: conn}, nil
}

func (c *adminClient) Close() error { return c.conn.Close() }

func buildTLS(cfg adminDialConfig) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.TLSCA)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA %s contains no usable certs", cfg.TLSCA)
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}), nil
}

// emitJSON writes a response to stdout in the admin JSON dialect.
//
// Every --format=json path goes through here rather than through
// encoding/json, so tidectl and tide cannot drift apart on what their output
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
