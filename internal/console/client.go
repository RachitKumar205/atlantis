package console

// adminClient dials the atlantis admin gRPC service using the same
// JSON-envelope wire format as cmd/tidectl/client.go. Duplicated rather
// than shared so cmd/tidectl stays lean with no import on internal/console.

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

type adminClient struct {
	conn *grpc.ClientConn
}

func dialAdmin(cfg Config) (*adminClient, error) {
	var creds credentials.TransportCredentials
	if cfg.ATLTLSCert != "" {
		c, err := buildAdminTLS(cfg.ATLTLSCert, cfg.ATLTLSKey, cfg.ATLTLSCA)
		if err != nil {
			return nil, err
		}
		creds = c
	} else {
		fmt.Fprintln(os.Stderr, "console: ATL_TLS_CERT not set — using insecure transport (dev only)")
		creds = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient(cfg.ATLEndpoint,
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.ForceCodecV2(jsonCodec{})),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.ATLEndpoint, err)
	}
	return &adminClient{conn: conn}, nil
}

func (c *adminClient) Close() error { return c.conn.Close() }

// invoke calls an admin RPC, marshalling req to JSON and unmarshalling the
// response into reply.
func (c *adminClient) invoke(ctx context.Context, method string, req, reply any) error {
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	in := jsonMsg{Raw: raw}
	var out jsonMsg
	if err := c.conn.Invoke(ctx, method, &in, &out); err != nil {
		return err
	}
	return json.Unmarshal(out.Raw, reply)
}

// invokeRaw calls an admin RPC and returns the raw JSON response bytes,
// which the HTTP handler can forward directly to the browser.
func (c *adminClient) invokeRaw(ctx context.Context, method string, req any) (json.RawMessage, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	in := jsonMsg{Raw: raw}
	var out jsonMsg
	if err := c.conn.Invoke(ctx, method, &in, &out); err != nil {
		return nil, err
	}
	return json.RawMessage(out.Raw), nil
}

func buildAdminTLS(certFile, keyFile, caFile string) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA %s contains no usable certs", caFile)
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
