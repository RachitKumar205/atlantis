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

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

type adminClient struct {
	adminpb.AdminServiceClient
	conn *grpc.ClientConn
}

// dialOrg opens an mTLS channel to one organisation's atlantis.
//
// Credentials come from the organisation's registry row rather than from files
// on disk, because there is now one set per organisation and they arrive while
// the process is running. The certificate and CA are PEM text from the row; the
// private key has just been decrypted by internal/console/secrets and should
// not outlive this call by any longer than the tls.Certificate it becomes.
//
// The CA is the boundary this whole step rests on. Each organisation's atlantis
// has its own trust root, so credentials issued for one do not chain at
// another: the mismatch is refused inside the TLS handshake, before any
// atlantis code runs and long before anything could return the wrong
// organisation's data.
//
// ServerName is deliberately left unset, so verification uses the authority
// derived from the endpoint — which means an organisation's server leaf must
// carry a SAN matching the address in its row. That is a provisioning
// requirement, and getting it wrong surfaces as a handshake failure naming the
// host, which is the right place to find out.
func dialOrg(creds *orgCredentials) (*adminClient, error) {
	tlsCreds, err := buildOrgTLS(creds)
	if err != nil {
		return nil, err
	}
	// No ForceCodecV2: the default proto codec applies. That option set the
	// content-subtype connection-wide, which is why a client could never mix
	// the JSON and protobuf paths per-RPC and had to move all at once.
	//
	// grpc.NewClient does not connect here — the first RPC does. So building a
	// channel for an organisation costs a key parse and nothing on the network,
	// which is what makes a pool of them cheap.
	conn, err := grpc.NewClient(creds.Endpoint,
		grpc.WithTransportCredentials(tlsCreds),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", creds.Endpoint, err)
	}
	return &adminClient{AdminServiceClient: adminpb.NewAdminServiceClient(conn), conn: conn}, nil
}

func (c *adminClient) Close() error { return c.conn.Close() }

func buildOrgTLS(creds *orgCredentials) (credentials.TransportCredentials, error) {
	cert, err := tls.X509KeyPair([]byte(creds.CertPEM), creds.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("client certificate for %s does not match its key: %w", creds.Org, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(creds.CAPEM)) {
		return nil, fmt.Errorf("CA for %s contains no usable certificates", creds.Org)
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}), nil
}
