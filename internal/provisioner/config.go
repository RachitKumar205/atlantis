// Package provisioner turns queued organisations into running ones, joining
// internal/cloud/store, internal/cloud/provision and internal/console.
//
// A separate binary from `cloud serve`: only this process holds Kubernetes
// credentials. They come from controller-runtime's config.GetConfig, which
// resolves in-cluster credentials first and falls back to KUBECONFIG and then
// ~/.kube/config, so one binary works in a cluster and on a laptop.
//
// Two provisioners are safe. The claim is one statement, a CTE with FOR UPDATE
// SKIP LOCKED feeding an UPDATE, so scale is more processes.
package provisioner

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/provision"
)

// Defaults for the loop's own timings.
//
// ReadyTimeout is the one provision.Config field this package sets rather than
// leaving empty. The lease is sized from it — a wait that outlives the claim
// hands the organisation to a second provisioner while the first is still
// working on it — so it has to be known here rather than defaulted inside
// NewKube.
const (
	DefaultPollInterval      = 10 * time.Second
	DefaultReconcileInterval = 5 * time.Minute
	DefaultReadyTimeout      = 5 * time.Minute
	DefaultHeartbeat         = 30 * time.Second
	DefaultRetryBase         = 30 * time.Second
	DefaultRetryMax          = 30 * time.Minute
	DefaultHealthAddr        = ":8082"

	// DefaultConsoleCertRenewWithin is how much life the console's certificate
	// must have left before a reconcile pass replaces it. Ten days is how long
	// this provisioner can be wedged before an organisation loses console
	// access.
	//
	// Paired with certs.ClientLifetime, thirty days. A window wider than the
	// lifetime rotates on every pass; a much narrower one removes the margin.
	DefaultConsoleCertRenewWithin = 10 * 24 * time.Hour

	// DefaultMetricsAddr is loopback. The address matters, not the port:
	// DefaultHealthAddr binds every interface so the kubelet can reach the
	// probes, and tenant namespaces restrict ingress rather than egress, so a
	// tenant workload can connect to anything it can address.
	//
	// Nothing scrapes this process today. A scraper arriving later needs this
	// listener to carry a credential.
	DefaultMetricsAddr = "127.0.0.1:9102"

	// leaseFactor sizes the default lease from ReadyTimeout. Three times leaves
	// room for the two Ensure calls either side of the wait, both of which do
	// real work, without making a crashed provisioner's organisation
	// unclaimable for an unreasonable time.
	leaseFactor = 3
)

// Config is everything this process needs to run.
type Config struct {
	// CloudPGURL is the queue, the organisations, and the audit log.
	CloudPGURL string

	// ConsolePGURL and ConsoleDataKey are what console.RegisterOrg needs. It
	// opens and closes its own pool, so these are a URL and a base64 keyset
	// rather than anything already open.
	ConsolePGURL   string
	ConsoleDataKey string

	// ConsoleURL is where a provisioned organisation's console lives, and is
	// what cloud.orgs.console_url is set to.
	//
	// Read from CLOUD_AUDIENCE rather than a setting of its own. It has to be
	// byte-identical to the value the console is configured with — Cloud mints
	// assertions for that audience and the console rejects anything else — and
	// `make dev-org-register` already passes CLOUD_AUDIENCE here for exactly
	// that reason. A second name for one value is two settings that must agree.
	ConsoleURL string

	// ClaimedBy names this process in the queue. Correctness rests on the
	// lease; this is what traces a wedged row back to a process.
	ClaimedBy string

	// PollInterval is how often an idle queue is checked. There is no
	// LISTEN/NOTIFY: provisioning takes minutes and happens rarely, so a
	// trigger channel would have no reader a ticker does not already cover.
	PollInterval time.Duration

	// ReconcileInterval is how often ready organisations are checked against
	// the cluster. Much slower than PollInterval: it lists every ready
	// organisation and asks the API server about each, so it is the one loop
	// whose cost grows with the number of customers.
	ReconcileInterval time.Duration

	// ConsoleCertRenewWithin is how close to expiry the console's credential
	// for an organisation may get before a reconcile pass reissues it. See
	// DefaultConsoleCertRenewWithin for how the number is chosen.
	ConsoleCertRenewWithin time.Duration

	// Lease is how long a claim is held before another provisioner may take it.
	// Heartbeat extends it while a wait is in progress.
	Lease     time.Duration
	Heartbeat time.Duration

	// RetryBase and RetryMax bound the backoff after a failure. Escalating,
	// capped: a permanent fault like a bad image reference must not become a
	// loop that provisions nothing and fills the log, and a transient cluster
	// problem must still recover unattended.
	RetryBase time.Duration
	RetryMax  time.Duration

	// HealthAddr serves /healthz and /readyz. It binds every interface,
	// because the kubelet probes it.
	HealthAddr string

	// MetricsAddr serves /metrics, and only that. Separate from HealthAddr so
	// the two trust levels are two listeners rather than one that has to be
	// open for the kubelet's sake. See DefaultMetricsAddr.
	MetricsAddr string

	// Provision is the deployment-shaped half, passed to provision.NewKube.
	Provision provision.Config
}

// ConfigFromEnv reads the whole configuration and reports everything wrong with
// it at once.
//
// One error naming every missing setting, so an operator configuring this from
// scratch does not learn about ten variables over ten restarts.
func ConfigFromEnv() (Config, error) {
	c := Config{
		CloudPGURL:     os.Getenv("CLOUD_PG_URL"),
		ConsolePGURL:   os.Getenv("CONSOLE_PG_URL"),
		ConsoleDataKey: os.Getenv("CONSOLE_DATA_KEY"),
		ConsoleURL:     strings.TrimRight(strings.TrimSpace(os.Getenv("CLOUD_AUDIENCE")), "/"),
		// Trimmed, so validate's empty check is a guard that can actually fire.
		// envOr only rejects the empty string, so " " would otherwise sail
		// through and put a blank-looking claimant on every row.
		ClaimedBy:         strings.TrimSpace(envOr("PROVISIONER_NAME", defaultName())),
		PollInterval:      envDuration("PROVISIONER_POLL_INTERVAL", DefaultPollInterval),
		ReconcileInterval: envDuration("PROVISIONER_RECONCILE_INTERVAL", DefaultReconcileInterval),
		ConsoleCertRenewWithin: envDuration("PROVISIONER_CONSOLE_CERT_RENEW_WITHIN",
			DefaultConsoleCertRenewWithin),
		Heartbeat:   envDuration("PROVISIONER_LEASE_HEARTBEAT", DefaultHeartbeat),
		RetryBase:   envDuration("PROVISIONER_RETRY_BASE", DefaultRetryBase),
		RetryMax:    envDuration("PROVISIONER_RETRY_MAX", DefaultRetryMax),
		HealthAddr:  envOr("PROVISIONER_HEALTH_LISTEN", DefaultHealthAddr),
		MetricsAddr: envOr("PROVISIONER_METRICS_LISTEN", DefaultMetricsAddr),

		Provision: provision.Config{
			// Set here because the lease is sized from it.
			ReadyTimeout: envDuration("PROVISIONER_READY_TIMEOUT", DefaultReadyTimeout),

			// Required — provision.Config.validate refuses without them, and
			// each is a value that cannot be guessed. A guessed host produces
			// certificates whose SAN matches nothing; a guessed image is a pod
			// that cannot start; a guessed cache address is a pod that never
			// becomes Ready.
			ExternalHost: os.Getenv("PROVISIONER_EXTERNAL_HOST"),

			// Defaults true: a hosted console runs in the cluster and dials a
			// Service name. False for a console on a developer's machine, which
			// is what `make dev-console-app` runs.
			//
			// A wrong value does not fail at startup. The console resolves a
			// name that does not exist where it is running, and every page
			// reports a DNS error naming the cluster resolver.
			ConsoleInCluster: envBool("PROVISIONER_CONSOLE_IN_CLUSTER", true),

			ServerImage:   os.Getenv("PROVISIONER_SERVER_IMAGE"),
			SignerImage:   os.Getenv("PROVISIONER_SIGNER_IMAGE"),
			PostgresImage: os.Getenv("PROVISIONER_POSTGRES_IMAGE"),
			MemcachedAddr: os.Getenv("PROVISIONER_MEMCACHED_ADDR"),

			// Everything below is left empty when unset rather than defaulted
			// here. provision.Config.withDefaults runs inside NewKube;
			// fallbacks here would be a second source of truth that wins,
			// because it runs first.
			NamespacePrefix:   os.Getenv("PROVISIONER_NAMESPACE_PREFIX"),
			StorageClass:      os.Getenv("PROVISIONER_STORAGE_CLASS"),
			PullPolicy:        os.Getenv("PROVISIONER_PULL_POLICY"),
			PodCIDR:           os.Getenv("PROVISIONER_POD_CIDR"),
			OperatorNamespace: os.Getenv("PROVISIONER_OPERATOR_NAMESPACE"),
			PostgresStorage:   os.Getenv("PROVISIONER_POSTGRES_STORAGE"),
			PostgresInstances: envInt32("PROVISIONER_POSTGRES_INSTANCES", 0),
		},
	}

	// The lease defaults from ReadyTimeout rather than from a constant, so
	// raising the timeout does not silently produce a lease it can outlive.
	c.Lease = envDuration("PROVISIONER_LEASE", leaseFactor*c.Provision.ReadyTimeout)

	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// validate refuses a configuration that would start and then not work.
func (c Config) validate() error {
	var missing []string
	for _, r := range []struct {
		name  string
		value string
	}{
		{"CLOUD_PG_URL", c.CloudPGURL},
		{"CONSOLE_PG_URL", c.ConsolePGURL},
		{"CONSOLE_DATA_KEY", c.ConsoleDataKey},
		{"CLOUD_AUDIENCE", c.ConsoleURL},
		{"PROVISIONER_EXTERNAL_HOST", c.Provision.ExternalHost},
		{"PROVISIONER_SERVER_IMAGE", c.Provision.ServerImage},
		{"PROVISIONER_SIGNER_IMAGE", c.Provision.SignerImage},
		{"PROVISIONER_POSTGRES_IMAGE", c.Provision.PostgresImage},
		{"PROVISIONER_MEMCACHED_ADDR", c.Provision.MemcachedAddr},
	} {
		if strings.TrimSpace(r.value) == "" {
			missing = append(missing, r.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the provisioner needs these and they are unset: %s",
			strings.Join(missing, ", "))
	}

	// CLOUD_AUDIENCE ends up in cloud.orgs.console_url, which carries a CHECK
	// constraint. Refusing here names the setting; letting it through produces
	// a constraint violation after the console row has already been written,
	// which is a half-registered organisation and a much worse message.
	if !strings.HasPrefix(c.ConsoleURL, "http://") && !strings.HasPrefix(c.ConsoleURL, "https://") {
		return fmt.Errorf("CLOUD_AUDIENCE must be an absolute http(s) URL, got %q: "+
			"it is written to cloud.orgs.console_url, which requires one", c.ConsoleURL)
	}

	// WaitReady can burn the entire ReadyTimeout. A lease shorter than that
	// expires mid-wait, and the row becomes claimable by a second provisioner
	// that starts from the top — while the first returns from its wait and
	// stamps the result over it, because MarkProvisioned is not guarded on
	// claimed_by. The heartbeat makes this unlikely; this makes it impossible
	// to configure.
	if c.Lease <= c.Provision.ReadyTimeout {
		return fmt.Errorf("PROVISIONER_LEASE (%s) must exceed PROVISIONER_READY_TIMEOUT (%s): "+
			"a lease that expires during the readiness wait lets a second provisioner "+
			"claim an organisation this one is still building",
			c.Lease, c.Provision.ReadyTimeout)
	}
	if c.Heartbeat >= c.Lease {
		return fmt.Errorf("PROVISIONER_LEASE_HEARTBEAT (%s) must be shorter than "+
			"PROVISIONER_LEASE (%s), or the lease expires before it is extended",
			c.Heartbeat, c.Lease)
	}

	// MarkProvisioningFailed refuses anything that rounds to zero milliseconds,
	// because "claimable immediately" is the busy loop the backoff exists to
	// prevent. Catching it here means the refusal names the setting rather than
	// arriving from the store on the first failure.
	if c.RetryBase.Milliseconds() <= 0 {
		return errors.New("PROVISIONER_RETRY_BASE must be at least a millisecond: " +
			"without a backoff a permanent failure becomes a busy loop")
	}
	if c.RetryMax < c.RetryBase {
		return fmt.Errorf("PROVISIONER_RETRY_MAX (%s) is below PROVISIONER_RETRY_BASE (%s)",
			c.RetryMax, c.RetryBase)
	}
	if c.PollInterval <= 0 {
		return errors.New("PROVISIONER_POLL_INTERVAL must be positive")
	}
	if c.ReconcileInterval <= 0 {
		return errors.New("PROVISIONER_RECONCILE_INTERVAL must be positive")
	}
	// Zero would mean rotating only a certificate that has already expired.
	if c.ConsoleCertRenewWithin <= 0 {
		return errors.New("PROVISIONER_CONSOLE_CERT_RENEW_WITHIN must be positive")
	}
	if c.ClaimedBy == "" {
		return errors.New("PROVISIONER_NAME must not be empty")
	}
	return nil
}

// backoff is the delay before an organisation that just failed is retried.
//
// Doubling from RetryBase, capped at RetryMax. attempts includes the attempt
// that just failed, so the first failure waits RetryBase rather than twice it.
func (c Config) backoff(attempts int) time.Duration {
	d := c.RetryBase
	for i := 1; i < attempts; i++ {
		d *= 2
		// `d <= 0` catches the doubling overflowing int64. A negative duration
		// arrives at MarkProvisioningFailed as "claimable immediately", which
		// it refuses, so the failure would be reported as an error about
		// recording the error and the original cause would be lost.
		if d >= c.RetryMax || d <= 0 {
			return c.RetryMax
		}
	}
	if d > c.RetryMax || d <= 0 {
		return c.RetryMax
	}
	return d
}

// defaultName identifies this process in the queue.
//
// The hostname is right in both deployments: locally the developer's machine,
// in Kubernetes the pod name, which is the argument to `kubectl logs`.
func defaultName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "provisioner"
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// envBool reads a boolean whose default may be true.
//
// Only "false", "0" and "no" turn one off, and anything unrecognised keeps the
// default rather than being read as false. A setting that defaults true must
// not be switched off by a typo: PROVISIONER_CONSOLE_IN_CLUSTER=flase would
// otherwise repoint every organisation's console address at a name the console
// cannot resolve, and nothing would report a bad value.
func envBool(name string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "":
		return def
	case "false", "0", "no":
		return false
	case "true", "1", "yes":
		return true
	default:
		fmt.Fprintf(os.Stderr, "provisioner: %s=%q is not a boolean (using %v)\n",
			name, os.Getenv(name), def)
		return def
	}
}

func envDuration(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		// The default rather than an error, matching envDuration in cmd/server
		// and internal/console: a malformed duration is a typo on a setting
		// that has a working default.
		return def
	}
	return d
}

func envInt32(name string, def int32) int32 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return def
	}
	return int32(n)
}
