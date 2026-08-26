package main

import (
	"strings"
	"testing"
	"time"
)

// setBootEnv sets the minimum a config must carry to load: a database and the
// mTLS material. Tests about anything else start from here so a new required
// variable is added in one place rather than found one failing test at a time.
//
// The paths need not exist — loadConfig validates that they are configured;
// transportCreds is what reads them.
func setBootEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PG_URL", "postgres://x")
	t.Setenv("TLS_CERT_FILE", "/etc/atlantis/server.crt")
	t.Setenv("TLS_KEY_FILE", "/etc/atlantis/server.key")
	t.Setenv("TLS_CA_FILE", "/etc/atlantis/ca.crt")
}

func TestLoadConfig_RejectsMissingPGURL(t *testing.T) {
	setBootEnv(t)
	t.Setenv("PG_URL", "")
	if _, err := loadConfig(); err == nil {
		t.Fatalf("expected error when PG_URL is empty")
	}
}

func TestLoadConfig_AppliesDefaults(t *testing.T) {
	setBootEnv(t)
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.GRPCAddr != ":9090" {
		t.Errorf("GRPCAddr: %s", c.GRPCAddr)
	}
	if c.PGMaxConns != 50 {
		t.Errorf("PGMaxConns: %d", c.PGMaxConns)
	}
	if c.CacheDefaultTTL != 10*time.Minute {
		t.Errorf("CacheDefaultTTL: %v", c.CacheDefaultTTL)
	}
	if c.OutboxDrainInterval != 250*time.Millisecond {
		t.Errorf("OutboxDrainInterval: %v", c.OutboxDrainInterval)
	}
	if len(c.MemcachedAddrs) != 1 || c.MemcachedAddrs[0] != "localhost:11211" {
		t.Errorf("MemcachedAddrs: %v", c.MemcachedAddrs)
	}
}

func TestLoadConfig_ParsesEnvVars(t *testing.T) {
	setBootEnv(t)
	t.Setenv("GRPC_LISTEN", ":1234")
	t.Setenv("PG_MAX_CONNS", "100")
	t.Setenv("MEMCACHED_ADDR", "a:11211, b:11211 ,c:11211")
	t.Setenv("CACHE_DEFAULT_TTL", "30m")

	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.GRPCAddr != ":1234" {
		t.Errorf("GRPCAddr: %s", c.GRPCAddr)
	}
	if c.PGMaxConns != 100 {
		t.Errorf("PGMaxConns: %d", c.PGMaxConns)
	}
	if len(c.MemcachedAddrs) != 3 ||
		c.MemcachedAddrs[0] != "a:11211" ||
		c.MemcachedAddrs[1] != "b:11211" ||
		c.MemcachedAddrs[2] != "c:11211" {
		t.Errorf("MemcachedAddrs: %v", c.MemcachedAddrs)
	}
	if c.CacheDefaultTTL != 30*time.Minute {
		t.Errorf("CacheDefaultTTL: %v", c.CacheDefaultTTL)
	}
}

// Every one of the three is required, and each is checked separately.
//
// A guard written as `if cert == "" ` would pass a test that only cleared
// TLS_CERT_FILE while letting a server boot with no key or no CA — which fails
// later, inside transportCreds, with a file error rather than a configuration
// one.
func TestLoadConfig_RequiresEveryTLSVariable(t *testing.T) {
	for _, missing := range []string{"TLS_CERT_FILE", "TLS_KEY_FILE", "TLS_CA_FILE"} {
		t.Run("without "+missing, func(t *testing.T) {
			setBootEnv(t)
			t.Setenv(missing, "")

			_, err := loadConfig()
			if err == nil {
				t.Fatalf("loadConfig accepted a config with no %s. The server would "+
					"boot, and the caller allowlist, cert binding and capability "+
					"checks are all keyed to the client identity mTLS provides",
					missing)
			}
			// The name of what is missing, and how to get it. An operator hitting
			// this at 3am has the error text and nothing else.
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("the error does not name %s, so it does not say which "+
					"variable to set: %v", missing, err)
			}
			if !strings.Contains(err.Error(), "make dev-certs") {
				t.Errorf("the error does not name the command that produces the "+
					"certificates: %v", err)
			}
		})
	}
}

// And the whole set present is accepted — the other half, without which a
// guard that refused every configuration would pass the test above.
func TestLoadConfig_AcceptsCompleteTLS(t *testing.T) {
	setBootEnv(t)
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig rejected a complete mTLS configuration: %v", err)
	}
	if c.TLSCertFile == "" || c.TLSKeyFile == "" || c.TLSCAFile == "" {
		t.Errorf("TLS paths did not survive loading: cert=%q key=%q ca=%q",
			c.TLSCertFile, c.TLSKeyFile, c.TLSCAFile)
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{" a , b ", []string{"a", "b"}},
		{",a,,b,", []string{"a", "b"}}, // empty fields skipped
	}
	for _, c := range cases {
		got := splitCSV(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitCSV(%q) len got %d want %d (%v)", c.in, len(got), len(c.want), got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitCSV(%q)[%d] = %q want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

// Nothing is exempt from cert binding unless an operator says so.
//
// A default of "atlantis-console" made sense while binding meant a stored leaf
// fingerprint the console had none of. Migration 0032 deleted the fingerprints,
// so that default now exempts the console from the only question the check
// still asks: does this caller have an identity row.
//
// The result is not a weaker check but no revocation. RevokeCaller deletes the
// row and the interceptor refuses within five seconds; an exempt caller skips
// that and cannot be cut off. The console holds an admin credential for every
// organisation.
//
// Reintroducing the default is a one-word edit that nothing else in the tree
// catches.
func TestNoCallerIsExemptFromCertBindingByDefault(t *testing.T) {
	setBootEnv(t)
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(c.CertBindingExemptCallers) != 0 {
		t.Errorf("CertBindingExemptCallers defaults to %v, but an exempt caller is one "+
			"RevokeCaller cannot cut off; exemptions must be an operator's deliberate act",
			c.CertBindingExemptCallers)
	}
}

// The exemption still works when an operator asks for it.
//
// The pin above is only half the property. A default of "none" that came from
// the parse being broken — rather than from the default being removed — would
// satisfy it while quietly retiring the escape hatch the bootstrap case needs.
func TestCertBindingExemptionIsStillAvailableToOperators(t *testing.T) {
	setBootEnv(t)
	t.Setenv("ATL_CERT_BINDING_EXEMPT_CALLERS", "bootstrap-cn, other-cn")
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(c.CertBindingExemptCallers) != 2 ||
		c.CertBindingExemptCallers[0] != "bootstrap-cn" ||
		c.CertBindingExemptCallers[1] != "other-cn" {
		t.Errorf("ATL_CERT_BINDING_EXEMPT_CALLERS parsed as %v, want the two CNs set",
			c.CertBindingExemptCallers)
	}
}
