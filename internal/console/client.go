package console

// adminClient dials the atlantis admin gRPC service over protobuf, wrapping
// the generated AdminServiceClient with the connection it owns.
//
// This was the last of four hand-maintained copies of the admin wire shapes —
// the others were the server, cmd/tide, and cmd/tidectl. Nothing checked that
// they agreed, so a field added on one side and forgotten on another was a
// silent mismatch that compiled. They are now all generated from
// atlantis/admin/v1/admin.proto.
//
// Responses reach the browser as canonical proto JSON via clients/go/adminjson,
// the same dialect tide and tidectl emit. See that package for what changes
// versus the old hand-marshalled shape — notably 64-bit integers are quoted and
// enums are their full names.

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

type adminClient struct {
	adminpb.AdminServiceClient
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
	// No ForceCodecV2: the default proto codec applies. That option set the
	// content-subtype connection-wide, which is why a client could never mix
	// the JSON and protobuf paths per-RPC and had to move all at once.
	conn, err := grpc.NewClient(cfg.ATLEndpoint,
		grpc.WithTransportCredentials(creds),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.ATLEndpoint, err)
	}
	return &adminClient{AdminServiceClient: adminpb.NewAdminServiceClient(conn), conn: conn}, nil
}

func (c *adminClient) Close() error { return c.conn.Close() }

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
