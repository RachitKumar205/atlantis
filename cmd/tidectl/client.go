package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/mem"
)

// adminClient is tidectl's hand-rolled gRPC client for the admin
// service's JSON-envelope RPCs. Mirrors cmd/tide/client.go — the wire
// shape is identical so both CLIs can talk to the same server. We
// duplicate rather than share a package because keeping the CLI
// binaries lean (no transitive on internal/server) is worth the 60-line
// copy.
type adminClient struct {
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
	conn, err := grpc.NewClient(cfg.Endpoint,
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.ForceCodecV2(jsonCodec{})),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.Endpoint, err)
	}
	return &adminClient{conn: conn}, nil
}

func (c *adminClient) Close() error { return c.conn.Close() }

func (c *adminClient) invoke(ctx context.Context, method string, req, reply any) error {
	rawReq, err := json.Marshal(req)
	if err != nil {
		return err
	}
	in := jsonMsg{Raw: rawReq}
	var out jsonMsg
	if err := c.conn.Invoke(ctx, method, &in, &out); err != nil {
		return err
	}
	return json.Unmarshal(out.Raw, reply)
}

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

type jsonMsg struct{ Raw []byte }

type jsonCodec struct{}

func (jsonCodec) Marshal(v any) (mem.BufferSlice, error) {
	m, ok := v.(*jsonMsg)
	if !ok {
		return nil, fmt.Errorf("jsonCodec: cannot marshal %T", v)
	}
	return mem.BufferSlice{mem.SliceBuffer(m.Raw)}, nil
}

func (jsonCodec) Unmarshal(data mem.BufferSlice, v any) error {
	m, ok := v.(*jsonMsg)
	if !ok {
		return fmt.Errorf("jsonCodec: cannot unmarshal into %T", v)
	}
	m.Raw = append(m.Raw[:0], data.Materialize()...)
	return nil
}

func (jsonCodec) Name() string { return "json" }
