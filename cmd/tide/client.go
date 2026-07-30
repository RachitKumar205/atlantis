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
// This used to be a hand-rolled JSON-envelope client with a custom codec, and
// each subcommand redeclared the message it sent. The shapes lived in four
// places — here, tidectl, the console, and the server — with nothing checking
// that they agreed. Now they are generated from
// atlantis/admin/v1/admin.proto, and a field the server adds is a field this
// client has.
type adminClient struct {
	adminpb.AdminServiceClient
	conn *grpc.ClientConn
}

func dial(cfg *tideConfig) (*adminClient, error) {
	var creds credentials.TransportCredentials
	if cfg.TLS.Cert != "" || cfg.TLS.CertPEM != "" {
		var err error
		creds, err = buildTLS(cfg)
		if err != nil {
			return nil, err
		}
	} else {
		fmt.Fprintln(os.Stderr, "tide: TLS not configured — using insecure transport (dev only)")
		creds = insecure.NewCredentials()
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
	// Source the leaf cert + key. Inline PEM wins when set (config
	// validation in loadPCConfig already rejected the both-set case);
	// otherwise fall back to the file-path variant.
	var (
		cert tls.Certificate
		err  error
	)
	if cfg.TLS.CertPEM != "" {
		cert, err = tls.X509KeyPair([]byte(cfg.TLS.CertPEM), []byte(cfg.TLS.KeyPEM))
		if err != nil {
			return nil, fmt.Errorf("parse TIDE_TLS_CERT_PEM / TIDE_TLS_KEY_PEM: %w", err)
		}
	} else {
		cert, err = tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
		if err != nil {
			return nil, fmt.Errorf("load client cert: %w", err)
		}
	}

	// CA trust anchor — same pattern.
	var caPEM []byte
	if cfg.TLS.CAPEM != "" {
		caPEM = []byte(cfg.TLS.CAPEM)
	} else {
		caPEM, err = os.ReadFile(cfg.TLS.CA)
		if err != nil {
			return nil, fmt.Errorf("read CA: %w", err)
		}
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		src := cfg.TLS.CA
		if cfg.TLS.CAPEM != "" {
			src = "TIDE_TLS_CA_PEM"
		}
		return nil, fmt.Errorf("CA %s contains no usable certs", src)
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}), nil
}
