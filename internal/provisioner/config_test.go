package provisioner

import (
	"strings"
	"testing"
	"time"
)

// The provisioner's settings, and the states they exist to make impossible.
//
// Each is a way to start a process that looks configured and provisions
// nothing, or provisions something unreachable. None fails at startup on its
// own, which is why validate checks them there.

// provisionerEnv is a complete configuration.
var provisionerEnv = map[string]string{
	"CLOUD_PG_URL":     "postgres://localhost/cloud",
	"CONSOLE_PG_URL":   "postgres://localhost/console",
	"CONSOLE_DATA_KEY": "a-base64-keyset",
	"CLOUD_AUDIENCE":   "http://localhost:3000",

	"PROVISIONER_EXTERNAL_HOST":  "atl-dev.test",
	"PROVISIONER_SERVER_IMAGE":   "atlantis-server:local",
	"PROVISIONER_SIGNER_IMAGE":   "atlantis-signer:local",
	"PROVISIONER_POSTGRES_IMAGE": "atlantis-pg:17.11",
	"PROVISIONER_MEMCACHED_ADDR": "memcached.atlantis-system.svc.cluster.local:11211",
}

func setProvisionerEnv(t *testing.T) {
	t.Helper()
	for k, v := range provisionerEnv {
		t.Setenv(k, v)
	}
	// Everything optional is cleared, so a value in the developer's own shell
	// cannot make a test pass that would fail in CI.
	for _, k := range []string{
		"PROVISIONER_NAME", "PROVISIONER_POLL_INTERVAL", "PROVISIONER_LEASE",
		"PROVISIONER_LEASE_HEARTBEAT", "PROVISIONER_RETRY_BASE", "PROVISIONER_RETRY_MAX",
		"PROVISIONER_HEALTH_LISTEN", "PROVISIONER_READY_TIMEOUT",
		"PROVISIONER_NAMESPACE_PREFIX", "PROVISIONER_STORAGE_CLASS", "PROVISIONER_PULL_POLICY",
		"PROVISIONER_POD_CIDR", "PROVISIONER_OPERATOR_NAMESPACE",
		"PROVISIONER_POSTGRES_STORAGE", "PROVISIONER_POSTGRES_INSTANCES",
		"PROVISIONER_ORG_DOMAIN",
	} {
		t.Setenv(k, "")
	}
}

// An organisation domain stands in for the shared host, and the two are not
// combined.
func TestAnOrganisationDomainReplacesTheSharedHost(t *testing.T) {
	setProvisionerEnv(t)
	t.Setenv("PROVISIONER_EXTERNAL_HOST", "")
	t.Setenv("PROVISIONER_ORG_DOMAIN", "example.dev")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("a domain without a shared host was refused: %v", err)
	}
	if got := cfg.Provision.Host("acme"); got != "acme.example.dev" {
		t.Errorf("Host(acme) = %q, want acme.example.dev", got)
	}

	setProvisionerEnv(t)
	t.Setenv("PROVISIONER_ORG_DOMAIN", "example.dev")
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "both set") {
		t.Errorf("a shared host and a domain together: err = %v, want a refusal naming both", err)
	}
}

func TestACompleteConfigurationIsAccepted(t *testing.T) {
	setProvisionerEnv(t)

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("a complete configuration was refused: %v", err)
	}
	if cfg.ClaimedBy == "" {
		t.Error("ClaimedBy is empty; a claim nobody can trace to a process is one " +
			"an operator staring at a wedged queue cannot act on")
	}
	if cfg.Lease <= cfg.Provision.ReadyTimeout {
		t.Errorf("the default lease (%s) does not outlive the default readiness wait (%s)",
			cfg.Lease, cfg.Provision.ReadyTimeout)
	}
}

// Every one-setting-missing case is refused, and the message names the setting.
func TestEveryRequiredSettingIsRefusedWhenMissing(t *testing.T) {
	for missing := range provisionerEnv {
		t.Run("without "+missing, func(t *testing.T) {
			setProvisionerEnv(t)
			t.Setenv(missing, "")

			_, err := ConfigFromEnv()
			if err == nil {
				t.Fatalf("ConfigFromEnv accepted a configuration with no %s", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("the error does not name %s: %v", missing, err)
			}
		})
	}
}

// The refusal names every missing setting, not just the first one found.
//
// An operator configuring this from scratch would otherwise restart the process
// once per variable, which is how a five-minute task becomes an afternoon.
func TestTheRefusalNamesEveryMissingSetting(t *testing.T) {
	setProvisionerEnv(t)
	for k := range provisionerEnv {
		t.Setenv(k, "")
	}
	t.Setenv("CLOUD_PG_URL", provisionerEnv["CLOUD_PG_URL"])

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("a database URL alone was accepted")
	}
	for name := range provisionerEnv {
		if name == "CLOUD_PG_URL" {
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %s: %v", name, err)
		}
	}
	if strings.Contains(err.Error(), "CLOUD_PG_URL") {
		t.Errorf("the error names a setting that is present, sending somebody to the wrong line: %v", err)
	}
}

// The eight settings provision.Config defaults are read and left EMPTY here.
//
// withDefaults is unexported and runs inside NewKube, so a fallback supplied
// here is a second source of truth that wins by running first, and the two
// diverge on the first change to either.
func TestDefaultedSettingsArePassedThroughEmpty(t *testing.T) {
	setProvisionerEnv(t)

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}

	for _, tc := range []struct {
		name string
		got  string
	}{
		{"NamespacePrefix", cfg.Provision.NamespacePrefix},
		{"StorageClass", cfg.Provision.StorageClass},
		{"PullPolicy", cfg.Provision.PullPolicy},
		{"PodCIDR", cfg.Provision.PodCIDR},
		{"OperatorNamespace", cfg.Provision.OperatorNamespace},
		{"PostgresStorage", cfg.Provision.PostgresStorage},
	} {
		if tc.got != "" {
			t.Errorf("%s = %q, want empty: defaulting it here creates a second "+
				"source of truth for a value provision.withDefaults owns", tc.name, tc.got)
		}
	}
	if cfg.Provision.PostgresInstances != 0 {
		t.Errorf("PostgresInstances = %d, want 0 for the same reason",
			cfg.Provision.PostgresInstances)
	}

	// And they ARE read when set, or this would pass against a config that
	// ignored them entirely.
	t.Setenv("PROVISIONER_NAMESPACE_PREFIX", "tenant-")
	t.Setenv("PROVISIONER_POSTGRES_INSTANCES", "3")
	cfg, err = ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Provision.NamespacePrefix != "tenant-" {
		t.Errorf("NamespacePrefix = %q, want tenant-", cfg.Provision.NamespacePrefix)
	}
	if cfg.Provision.PostgresInstances != 3 {
		t.Errorf("PostgresInstances = %d, want 3", cfg.Provision.PostgresInstances)
	}
}

// A lease that can expire during the readiness wait is refused.
//
// This is the whole reason the lease is sized from ReadyTimeout. WaitReady can
// burn the entire timeout; a shorter lease expires mid-wait and the row becomes
// claimable by a second provisioner — while the first returns and stamps its
// result over the top, because MarkProvisioned is not guarded on claimed_by.
func TestALeaseShorterThanTheReadinessWaitIsRefused(t *testing.T) {
	setProvisionerEnv(t)
	t.Setenv("PROVISIONER_READY_TIMEOUT", "10m")
	t.Setenv("PROVISIONER_LEASE", "5m")

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("a lease that expires during the readiness wait was accepted")
	}
	if !strings.Contains(err.Error(), "PROVISIONER_LEASE") {
		t.Errorf("the error does not name the setting: %v", err)
	}
}

// Raising the readiness timeout raises the default lease with it.
//
// The default is computed from ReadyTimeout rather than fixed, so lengthening
// the wait cannot produce a lease the wait outlives.
func TestTheDefaultLeaseFollowsTheReadinessTimeout(t *testing.T) {
	setProvisionerEnv(t)
	t.Setenv("PROVISIONER_READY_TIMEOUT", "20m")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("raising the readiness timeout was refused: %v", err)
	}
	if cfg.Lease <= 20*time.Minute {
		t.Errorf("lease = %s, want more than the 20m readiness timeout it must outlive", cfg.Lease)
	}
}

// A heartbeat at or above the lease cannot keep it alive.
func TestAHeartbeatSlowerThanTheLeaseIsRefused(t *testing.T) {
	setProvisionerEnv(t)
	t.Setenv("PROVISIONER_LEASE", "10m")
	t.Setenv("PROVISIONER_LEASE_HEARTBEAT", "10m")

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("a heartbeat that fires no sooner than the lease expires was accepted")
	}
	if !strings.Contains(err.Error(), "PROVISIONER_LEASE_HEARTBEAT") {
		t.Errorf("the error does not name the setting: %v", err)
	}
}

// The console URL must be absolute, and is trimmed the way the store trims it.
//
// It ends up in cloud.orgs.console_url, which carries a CHECK constraint.
// Refusing here names the setting; letting it through produces a constraint
// violation after the console row has already been written — a half-registered
// organisation, and a much worse message.
func TestTheConsoleURLMustBeAbsolute(t *testing.T) {
	setProvisionerEnv(t)
	t.Setenv("CLOUD_AUDIENCE", "localhost:3000")

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("a console URL with no scheme was accepted")
	}
	if !strings.Contains(err.Error(), "CLOUD_AUDIENCE") {
		t.Errorf("the error does not name the setting: %v", err)
	}
}

func TestTheConsoleURLIsTrimmed(t *testing.T) {
	setProvisionerEnv(t)
	t.Setenv("CLOUD_AUDIENCE", "  http://localhost:3000/  ")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.ConsoleURL != "http://localhost:3000" {
		t.Errorf("ConsoleURL = %q, want it trimmed to match what SetConsoleURL stores — "+
			"CLOUD_AUDIENCE must be byte-identical to what the console is configured with",
			cfg.ConsoleURL)
	}
}

// MarkProvisioningFailed refuses a sub-millisecond delay too. Catching it here
// means the refusal names the setting rather than arriving from the store on
// the first failure.
func TestASubMillisecondRetryBaseIsRefused(t *testing.T) {
	setProvisionerEnv(t)
	t.Setenv("PROVISIONER_RETRY_BASE", "500us")

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("a retry delay that rounds to zero was accepted: it means claimable immediately")
	}
	if !strings.Contains(err.Error(), "PROVISIONER_RETRY_BASE") {
		t.Errorf("the error does not name the setting: %v", err)
	}
}

// The backoff escalates and then caps.
//
// Without escalation, a permanent fault becomes a loop that provisions nothing
// and fills the log. Without a cap, a transient problem takes hours to recover
// from unattended. atlantis.jobs has neither, which is why parked_objects is
// the model here rather than the queue this one otherwise copies.
func TestTheBackoffEscalatesAndCaps(t *testing.T) {
	cfg := Config{RetryBase: time.Second, RetryMax: 30 * time.Second}

	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second},  // capped
		{50, 30 * time.Second}, // still capped, not overflowed
	} {
		if got := cfg.backoff(tc.attempts); got != tc.want {
			t.Errorf("backoff(%d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}
}

// A whitespace-only name is refused, not stored.
//
// envOr only rejects the empty string, so without trimming this reaches the
// queue as a claimant that looks blank in every tool that prints it — and the
// empty check in validate would be a guard that could never fire.
func TestAWhitespaceOnlyNameIsRefused(t *testing.T) {
	setProvisionerEnv(t)
	t.Setenv("PROVISIONER_NAME", "   ")

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("a claimant name of pure whitespace was accepted")
	}
	if !strings.Contains(err.Error(), "PROVISIONER_NAME") {
		t.Errorf("the error does not name the setting: %v", err)
	}
}

// The backoff survives a RetryMax large enough to overflow the doubling.
//
// Absurd as configuration, and cheap to be right about: the doubling is int64
// nanoseconds, and an overflow produces a negative duration that
// MarkProvisioningFailed refuses — so the provisioner would report an error
// about recording an error, losing the cause that mattered.
func TestTheBackoffDoesNotOverflow(t *testing.T) {
	cfg := Config{RetryBase: time.Millisecond, RetryMax: time.Duration(1<<62 - 1)}
	for _, attempts := range []int{60, 62, 64, 100, 1000} {
		got := cfg.backoff(attempts)
		if got <= 0 {
			t.Errorf("backoff(%d) = %s, which MarkProvisioningFailed refuses", attempts, got)
		}
		if got > cfg.RetryMax {
			t.Errorf("backoff(%d) = %s, above RetryMax %s", attempts, got, cfg.RetryMax)
		}
	}
}

// A backoff is never zero, whatever it is asked for.
//
// backoff feeds MarkProvisioningFailed, which refuses anything under a
// millisecond — so a zero here would turn every failure into an error about
// recording the failure, which is two problems reported as one.
func TestTheBackoffIsNeverZero(t *testing.T) {
	cfg := Config{RetryBase: time.Second, RetryMax: time.Minute}
	for _, attempts := range []int{0, -1, 1, 100} {
		if got := cfg.backoff(attempts); got.Milliseconds() <= 0 {
			t.Errorf("backoff(%d) = %s, which MarkProvisioningFailed refuses", attempts, got)
		}
	}
}
