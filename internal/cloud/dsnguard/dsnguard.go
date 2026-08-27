// Package dsnguard opens a connection to a database somebody else owns.
//
// Cloud dials an address a request supplied, with a credential a request
// supplied. That is server-side request forgery unless every address is checked,
// so this refuses anything that is not a public host and pins what pgx dials.
//
// The DSN passes through and is not kept. Nothing here logs it, and Redact
// exists so an error carrying one never reaches a caller intact.
package dsnguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ConnectTimeout bounds the dial. A host that accepts and never answers holds a
// request open, and this endpoint is reachable by anyone with an account.
const ConnectTimeout = 10 * time.Second

// StatementTimeout bounds every query the session runs, server-side. Set as a
// runtime parameter so it applies to introspection this package never sees.
const StatementTimeout = "15s"

// ErrUnixSocket is returned for a DSN naming a socket directory.
//
// pgconn skips LookupFunc entirely when the host is an absolute path — see the
// isAbsolutePath branch in pgconn.Connect — so a socket path reaches the dialer
// without passing any address check. On this host that is Cloud's own Postgres.
var ErrUnixSocket = errors.New(
	"a unix socket path is not a database this can reach: supply a host and port")

// ErrNoTLS is returned for a DSN that disables TLS outright.
//
// pgx renders sslmode as a TLSConfig per candidate host, and nil means
// plaintext. A password crossing the internet in clear is not something to
// accept on the customer's behalf.
var ErrNoTLS = errors.New(
	"sslmode=disable would send the password in clear: use sslmode=require or stronger")

// Config parses dsn and returns a pool configuration that can reach only a
// public Postgres, over TLS.
//
// Refusals happen here, before anything is dialled. What survives is pinned:
// LookupFunc is the only resolver pgx uses, so no name is resolved twice and a
// record that changes between the check and the connection changes nothing.
func Config(dsn string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// The parse error can quote the DSN back.
		return nil, fmt.Errorf("that connection string could not be parsed: %w", Redact(err, dsn))
	}
	cc := cfg.ConnConfig

	// Every candidate, not just the first. A DSN may carry `host=a,b`, and
	// pgconn walks each one as a fallback until it connects — so a check on
	// Host alone leaves the rest unguarded.
	if isSocketPath(cc.Host) {
		return nil, ErrUnixSocket
	}
	for _, fb := range cc.Fallbacks {
		if isSocketPath(fb.Host) {
			return nil, ErrUnixSocket
		}
	}

	// sslmode=prefer, the libpq default, produces one candidate with TLS and
	// one without, so requiring TLS everywhere would refuse the commonest DSN
	// anybody pastes. Dropping the plaintext candidates makes prefer behave as
	// require: the connection is encrypted or it does not happen.
	//
	// Nothing survives only when TLS was disabled outright, which is ErrNoTLS.
	kept := cc.Fallbacks[:0]
	for _, fb := range cc.Fallbacks {
		if fb.TLSConfig != nil {
			kept = append(kept, fb)
		}
	}
	cc.Fallbacks = kept
	if cc.TLSConfig == nil {
		if len(cc.Fallbacks) == 0 {
			return nil, ErrNoTLS
		}
		// The primary was the plaintext half of prefer. Promote a TLS
		// candidate into its place so the first attempt is already encrypted.
		cc.Host, cc.Port, cc.TLSConfig = cc.Fallbacks[0].Host, cc.Fallbacks[0].Port, cc.Fallbacks[0].TLSConfig
		cc.Fallbacks = cc.Fallbacks[1:]
	}

	cc.ConnectTimeout = ConnectTimeout
	if cc.RuntimeParams == nil {
		cc.RuntimeParams = map[string]string{}
	}
	cc.RuntimeParams["statement_timeout"] = StatementTimeout

	// The only resolver pgx uses. Returning solely public addresses means there
	// is no second resolution for a rebinding answer to win.
	cc.LookupFunc = lookupPublic

	// The backstop, and the only thing that sees the final address on every
	// path. A literal IP in the DSN still passes through LookupFunc today, and
	// this does not depend on that staying true.
	cc.DialFunc = dialPublic

	return cfg, nil
}

// isSocketPath reports whether pgconn would treat the host as a unix socket
// directory, which is any absolute path.
func isSocketPath(host string) bool {
	return len(host) > 0 && host[0] == '/'
}

// lookupPublic resolves host and returns only the addresses that may be dialled.
func lookupPublic(ctx context.Context, host string) ([]string, error) {
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range addrs {
		if allowed(net.ParseIP(a)) == nil {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s resolves to no address this can reach: %w", host, ErrNotPublic)
	}
	return out, nil
}

// dialPublic checks the address it is handed and dials it.
func dialPublic(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if err := allowed(net.ParseIP(host)); err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: ConnectTimeout}
	return d.DialContext(ctx, network, addr)
}

// ErrNotPublic is returned for an address on this network rather than the
// internet.
var ErrNotPublic = errors.New("that address is not reachable from here")

// cgnat is the carrier-grade NAT range, which IsPrivate does not cover and
// which routes to somebody else's infrastructure.
var cgnat = net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// allowed reports whether an address belongs to the public internet.
//
// Everything else is refused, including the ranges that make this an SSRF: the
// loopback that reaches this process, the private ranges that reach the rest of
// the cluster, and 169.254.0.0/16, which is where a cloud provider serves
// instance credentials.
//
// Refuses an unparseable address rather than passing it on. A dialer given a
// string this could not read is a dialer resolving it again.
func allowed(ip net.IP) error {
	if ip == nil {
		return ErrNotPublic
	}
	switch {
	case ip.IsLoopback(),
		ip.IsPrivate(),
		ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(),
		ip.IsMulticast(),
		ip.IsUnspecified(),
		cgnat.Contains(ip):
		return fmt.Errorf("%s: %w", ip, ErrNotPublic)
	}
	return nil
}

// Redact removes a DSN from an error's text.
//
// pgx quotes the connection string in several of its errors, and the password
// is inside it. An error that reaches a log or a browser with one intact has
// published a credential, and neither place forgets.
func Redact(err error, dsn string) error {
	if err == nil || dsn == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, dsn) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, dsn, "[connection string removed]"))
}
