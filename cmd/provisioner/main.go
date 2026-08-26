// Command provisioner turns queued organisations into running ones.
//
// It claims from cloud.org_provisioning, builds the organisation in Kubernetes,
// registers it with the console, points Cloud at that console, and writes an
// audit row. Nothing else in the tree calls internal/cloud/provision — until
// this binary existed, provisioning was a package with no importer outside its
// own tests, and creating an organisation left it queued for nobody.
//
// Separate from `cloud serve` because it needs Kubernetes credentials and
// `cloud serve` holds every password, every TOTP secret and the assertion
// signing key. See internal/provisioner.
//
// Running more than one is safe. The claim is one statement with FOR UPDATE
// SKIP LOCKED, and a provisioner that dies mid-work frees its organisation when
// the lease expires, so there is no sweeper to run.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/rachitkumar205/atlantis/internal/cloud/provision"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
	"github.com/rachitkumar205/atlantis/internal/provisioner"
)

// shutdownGrace bounds the wait for the health listener to close.
const shutdownGrace = 15 * time.Second

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("provisioner refused to start", "err", err)
		os.Exit(1)
	}
}

// run is main with the exits taken out, so a test can boot the provisioner and
// read back why it refused.
//
// cmd/signer and cmd/server are split the same way for the same reason: a
// startup guard that only ever calls os.Exit can be asserted on by a subprocess
// test at best, and not at all from inside the package.
func run(log *slog.Logger) error {
	cfg, err := provisioner.ConfigFromEnv()
	if err != nil {
		return err
	}

	// The signal context is taken before anything is opened, so an interrupt
	// during a slow startup stops rather than being noticed afterwards.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openCloud(ctx, cfg.CloudPGURL, log)
	if err != nil {
		return err
	}
	defer db.Close()

	// A factory, not a client: it is called again whenever the cluster refuses
	// this process's credentials, and GetConfig re-reads the kubeconfig or the
	// service account token at that moment. A client built once survives
	// neither a rotated authority nor a development cluster that was rebuilt.
	newCluster := func() (provisioner.Cluster, error) { return openCluster(cfg.Provision, log) }

	w, err := provisioner.New(cfg, db, newCluster, nil, nil, log)
	if err != nil {
		return err
	}

	// Readiness is both dependencies, not just the database. A provisioner that
	// can read its queue and cannot reach Kubernetes provisions nothing, and
	// without this it would report itself ready while doing so.
	ready := func(ctx context.Context) error {
		if err := db.Pool().Ping(ctx); err != nil {
			return err
		}
		if !w.Healthy() {
			return errors.New("the cluster has refused this provisioner's credentials")
		}
		return nil
	}
	health := provisioner.NewHealthServer(cfg.HealthAddr, ready, ctx)
	go func() {
		log.Info("health http listening", "addr", cfg.HealthAddr)
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Stopping the process rather than serving on: a provisioner whose
			// health listener never came up is one Kubernetes cannot restart
			// when it wedges, which is worse than one that refuses now.
			log.Error("health http server", "err", err)
			stop()
		}
	}()

	// Metrics on their own listener, which is the whole point rather than tidy
	// separation: the health listener has to accept connections from anywhere
	// so the kubelet can probe it, and this one does not.
	//
	// A failure here does NOT stop the process, which is the opposite of the
	// decision above and deliberate. Kubernetes cannot restart a provisioner
	// whose probes never came up, so that failure has to be fatal. Nothing
	// scrapes this one — losing it costs observability, and killing a working
	// provisioner over it would trade the job for the telemetry about the job.
	metrics := provisioner.NewMetricsServer(cfg.MetricsAddr)
	go func() {
		log.Info("metrics http listening", "addr", cfg.MetricsAddr)
		if err := metrics.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics http server", "err", err)
		}
	}()

	runErr := w.Run(ctx)

	// Detached from ctx deliberately: ctx is already cancelled by the time this
	// runs, and Shutdown on a cancelled context returns instantly without
	// closing anything.
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	_ = health.Shutdown(shutCtx)
	_ = metrics.Shutdown(shutCtx)

	return runErr
}

// openCloud is the three-call sequence cmd/cloud uses, and all three matter.
//
// store.New does not migrate, so a fresh database would fail its first query
// rather than at startup. And VerifyPolicies is what refuses to serve on a
// schema where a table carries per-user rows with no boundary and no recorded
// decision — it runs on every open in cmd/cloud, and skipping it here would
// make the provisioner the one process that does not check.
func openCloud(ctx context.Context, pgURL string, log *slog.Logger) (*store.Store, error) {
	if err := store.Migrate(pgURL, log); err != nil {
		return nil, fmt.Errorf("migrate cloud schema: %w", err)
	}
	db, err := store.New(ctx, pgURL, log)
	if err != nil {
		return nil, fmt.Errorf("open cloud db: %w", err)
	}
	if err := store.VerifyPolicies(ctx, db.Pool()); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// openCluster builds the Kubernetes client and the provisioning target.
//
// GetConfig resolves in-cluster credentials first and falls back to KUBECONFIG
// and then ~/.kube/config, so the same binary works on a laptop and in a pod
// without a flag choosing between them.
//
// NewScheme is not optional and nothing forces it: the client is passed into
// NewKube rather than built there, so a client built with the default scheme
// compiles, runs, and then fails to encode a CloudNativePG Cluster.
func openCluster(cfg provision.Config, log *slog.Logger) (*provision.Kube, error) {
	restCfg, err := ctrlconfig.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("no Kubernetes configuration (set KUBECONFIG, or run in-cluster): %w", err)
	}
	scheme, err := provision.NewScheme()
	if err != nil {
		return nil, err
	}
	c, err := ctrlclient.New(restCfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes client: %w", err)
	}
	// NewKube is the only place provision.Config is defaulted and validated, so
	// this is also where a missing image reference or host is refused.
	return provision.NewKube(cfg, c, log)
}
