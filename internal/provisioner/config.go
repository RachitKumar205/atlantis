// Package provisioner turns queued organisations into running ones.
//
// It is the process that closes the chain. internal/cloud/store knows an
// organisation is waiting; internal/cloud/provision knows how to build one;
// internal/console knows how to record one. Until this package existed, nothing
// called the second of those from anywhere — provisioning was a package with no
// importer outside its own tests.
//
// # Why this is a binary and not a goroutine in cloud serve
//
// It holds Kubernetes credentials. cloud serve holds every password, every TOTP
// secret and the assertion signing key, and W3 deliberately made it a React
// origin with script-src 'self'. That risk was accepted on the understanding
// that script on Cloud's origin could reach /api/account/*. It was not accepted
// on the understanding that it could schedule pods.
//
// # Two provisioners are safe
//
// The claim is one statement — a CTE with FOR UPDATE SKIP LOCKED feeding an
// UPDATE — so concurrent claimants skip rows already held rather than re-reading
// them. Scale is more processes, not more goroutines here.
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
// leaving empty, and the reason is the lease: a wait that outlives the claim
// hands the organisation to a second provisioner while the first is still
// working on it. The lease has to be sized against this number, so this number
// has to be known here rather than defaulted out of sight inside NewKube.
const (
	DefaultPollInterval      = 10 * time.Second
	DefaultReconcileInterval = 5 * time.Minute
	DefaultReadyTimeout      = 5 * time.Minute
	DefaultHeartbeat         = 30 * time.Second
	DefaultRetryBase         = 30 * time.Second
	DefaultRetryMax          = 30 * time.Minute
	DefaultHealthAddr        = ":8082"

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

	// ClaimedBy names this process in the queue. Not load-bearing for
	// correctness, which is the lease's job, but a wedged row whose claimant is
	// blank is one nobody can trace to a process.
	ClaimedBy string

	// There is deliberately no Kubeconfig field.
	//
	// controller-runtime's config.GetConfig already resolves in-cluster
	// credentials first and falls back to KUBECONFIG and then ~/.kube/config,
	// which is exactly the behaviour that lets one binary work in both places
	// without a flag deciding which. A field here would be read from the
	// environment, stored, and never consulted — an inert setting that looks
	// like configuration, which is the shape CONSOLE_ENROLL_CLIENT_CA was
	// retired for.

	// PollInterval is how often an idle queue is checked. There is no
	// LISTEN/NOTIFY: provisioning takes minutes and happens rarely, so a ticker
	// is honest and a trigger channel would be machinery with no reader.
	PollInterval time.Duration

	// ReconcileInterval is how often ready organisations are checked against
	// the cluster. Much slower than PollInterval: it lists every ready
	// organisation and asks the API server about each, so it is the one loop
	// whose cost grows with the number of customers.
	ReconcileInterval time.Duration

	// Lease is how long a claim is held before another provisioner may take it.
	// Heartbeat extends it while a wait is in progress.
	Lease     time.Duration
	Heartbeat time.Duration

	// RetryBase and RetryMax bound the backoff after a failure. Escalating,
	// capped: a permanent fault like a bad image reference must not become a
	// loop that provisions nothing and fills the log, and a transient cluster
	// problem must still recover with nobody watching.
	RetryBase time.Duration
	RetryMax  time.Duration

	// HealthAddr serves /healthz, /readyz and /metrics.
	HealthAddr string

	// Provision is the deployment-shaped half, passed to provision.NewKube.
	Provision provision.Config
}

// ConfigFromEnv reads the whole configuration and reports everything wrong with
// it at once.
//
// One error naming every missing setting, not one per restart. An operator
// configuring this from scratch would otherwise learn about ten variables over
// ten restarts, which is how a five-minute task becomes an afternoon — the same
// reasoning validateEnrollment records in internal/console/config.go.
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
		Heartbeat:         envDuration("PROVISIONER_LEASE_HEARTBEAT", DefaultHeartbeat),
		RetryBase:         envDuration("PROVISIONER_RETRY_BASE", DefaultRetryBase),
		RetryMax:          envDuration("PROVISIONER_RETRY_MAX", DefaultRetryMax),
		HealthAddr:        envOr("PROVISIONER_HEALTH_LISTEN", DefaultHealthAddr),

		Provision: provision.Config{
			// Set here, deliberately, because the lease is sized from it.
			ReadyTimeout: envDuration("PROVISIONER_READY_TIMEOUT", DefaultReadyTimeout),

			// Required — provision.Config.validate refuses without them, and
			// each is a value that cannot be guessed. A guessed host produces
			// certificates whose SAN matches nothing; a guessed image is a pod
			// that cannot start; a guessed cache address is a pod that never
			// becomes Ready.
			ExternalHost:  os.Getenv("PROVISIONER_EXTERNAL_HOST"),
			ServerImage:   os.Getenv("PROVISIONER_SERVER_IMAGE"),
			SignerImage:   os.Getenv("PROVISIONER_SIGNER_IMAGE"),
			PostgresImage: os.Getenv("PROVISIONER_POSTGRES_IMAGE"),
			MemcachedAddr: os.Getenv("PROVISIONER_MEMCACHED_ADDR"),

			// Everything below is read and left EMPTY when unset, rather than
			// defaulted here.
			//
			// provision.Config.withDefaults is unexported and runs inside
			// NewKube. Supplying fallbacks here would create a second source of
			// truth for each of these, and two sources of truth drift — with
			// this one silently winning, because it runs first.
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

	// The guard the whole lease design rests on.
	//
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
		// `d <= 0` catches the doubling overflowing int64, which needs an
		// absurd RetryMax to reach but is worth closing here rather than in
		// the store: a negative duration arrives at MarkProvisioningFailed as
		// "claimable immediately", which it refuses — so a permanent fault
		// would start reporting an error about recording the error, and the
		// original cause would be the one that got lost.
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
// The hostname is right in both deployments: locally it is the developer's
// machine, and in Kubernetes it is the pod name, which is exactly what somebody
// reading a stuck row wants to `kubectl logs`.
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

func envDuration(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		// Deliberately the default rather than an error, matching envDuration
		// in cmd/server and internal/console. A malformed duration is a typo,
		// and the alternative — refusing to boot — is a worse trade for a
		// setting that has a working default.
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
