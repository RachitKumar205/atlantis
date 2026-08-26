package console

// adminClient dials the atlantis admin gRPC service over protobuf, wrapping
// the generated AdminServiceClient with the connection it owns.
//
// Wire shapes are generated from atlantis/admin/v1/admin.proto, shared with the
// server, cmd/tide and cmd/tidectl. Responses reach the browser as canonical
// proto JSON via clients/go/adminjson: 64-bit integers are quoted and enums are
// their full names.

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

// dialOrg opens an mTLS channel to one organisation's atlantis. Credentials
// come from its registry row, not from disk; the key has just been decrypted by
// internal/secrets and should not outlive the tls.Certificate it becomes.
//
// The CA is the boundary: each organisation has its own trust root, so
// credentials issued for one do not chain at another. ServerName is left unset,
// so the server leaf must carry a SAN matching the address in its row.
func dialOrg(creds *orgCredentials) (*adminClient, error) {
	tlsCreds, err := buildOrgTLS(creds)
	if err != nil {
		return nil, err
	}
	// No ForceCodecV2, so the default proto codec applies. That option sets the
	// content-subtype connection-wide, which is why a client cannot mix codecs
	// per RPC and has to move every call on a dial at once.
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
