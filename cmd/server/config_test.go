package main

import (
	"testing"
	"time"
)

func TestLoadConfig_RejectsMissingPGURL(t *testing.T) {
	t.Setenv("PG_URL", "")
	if _, err := loadConfig(); err == nil {
		t.Fatalf("expected error when PG_URL is empty")
	}
}

func TestLoadConfig_AppliesDefaults(t *testing.T) {
	t.Setenv("PG_URL", "postgres://x")
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
	t.Setenv("PG_URL", "postgres://x")
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

func TestLoadConfig_RejectsPartialTLS(t *testing.T) {
	t.Setenv("PG_URL", "postgres://x")
	t.Setenv("TLS_CERT_FILE", "/etc/cert")
	// KEY and CA absent — partial config.
	if _, err := loadConfig(); err == nil {
		t.Fatalf("expected error on partial TLS config")
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
