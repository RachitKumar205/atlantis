// atlantis gRPC server entrypoint.
//
// Composition root. Reads env config, builds the runtime tier (pgx pool,
// memcached client, reader, outbox, invalidation worker), constructs the
// gRPC server with mTLS + interceptors + health + reflection, and runs
// until the process is signaled.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/auth"
	"github.com/rachitkumar205/atlantis/internal/backfill"
	"github.com/rachitkumar205/atlantis/internal/cache/invalidate"
	"github.com/rachitkumar205/atlantis/internal/cache/memcached"
	"github.com/rachitkumar205/atlantis/internal/cache/queryresult"
	"github.com/rachitkumar205/atlantis/internal/cache/read"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/dsl/sqlvalidate"
	"github.com/rachitkumar205/atlantis/internal/migrate"
	"github.com/rachitkumar205/atlantis/internal/obs"
	"github.com/rachitkumar205/atlantis/internal/schema"
	"github.com/rachitkumar205/atlantis/internal/server/admin"
	"github.com/rachitkumar205/atlantis/internal/server/authz"
	"github.com/rachitkumar205/atlantis/internal/server/entity"
	"github.com/rachitkumar205/atlantis/internal/server/interceptors"
	"github.com/rachitkumar205/atlantis/internal/server/jobsdispatcher"
	"github.com/rachitkumar205/atlantis/internal/storage/pg"
	"github.com/rachitkumar205/atlantis/jobs"
	"github.com/rachitkumar205/atlantis/migrations"
)

// podID returns the local pod identifier used in the dispatcher's
// claimed_by formatting. Hostname-pid mirrors DefaultConfig in
// clients/go/jobs.
func podID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "atlantis"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// version is stamped at build time via -ldflags "-X main.version=...".
// Unstamped local builds report "dev".
var version = "dev"

func main() {
	cfg, err := loadConfig()
	if err != nil {
		// Use stderr directly — the logger isn't built yet.
		_, _ = os.Stderr.WriteString("config: " + err.Error() + "\n")
		os.Exit(2)
	}

	log, logRing := buildLogger(cfg)
	log.Info("atlantis starting", "version", version)

	// Refuse to start if any Admin RPC declares no required capability.
	//
	// This runs before anything is listening, and before the database is
	// touched, because the failure it catches is "an endpoint reachable with
	// no authorization at all" — the one condition where continuing to boot
	// is strictly worse than not starting. It is the same check the package's
	// tests run, so the build normally fails long before a deploy does; this
	// is the backstop for a binary assembled some other way.
	adminPolicy, err := authz.AdminPolicy()
	if err != nil {
		log.Error("refusing to start: admin authorization policy is incomplete", "err", err)
		os.Exit(2)
	}
	log.Info("admin authorization policy validated", "methods", len(adminPolicy.Methods()))

	// Top-level context cancels on SIGINT / SIGTERM.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, log, logRing, adminPolicy); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

// run is the top-level orchestrator. Resources are constructed in
// dependency order so each Close-defer runs LIFO at the end: pool is
// created first so the invalidation worker (which acquires a connection)
// can register cleanup before its Run starts.
func run(ctx context.Context, cfg config, log *slog.Logger, logRing *obs.LogRing, adminPolicy *authz.Policy) error {
	if cfg.AutoMigrate {
		// infra travels inside this binary; the tidectl tree is read from
		// MigrationsDir because the deployment emits it after the build. See
		// the migrations package for the split.
		if err := migrate.Run(cfg.PGURL, migrations.Infra, cfg.MigrationsDir, log); err != nil {
			return err
		}
	}

	pool, err := pg.New(ctx, pg.Config{
		URL:                 cfg.PGURL,
		MaxConns:            cfg.PGMaxConns,
		MinConns:            cfg.PGMinConns,
		MaxConnIdleTime:     cfg.PGMaxConnIdle,
		MaxConnLifetime:     cfg.PGMaxConnLifetime,
		HealthCheckPeriod:   cfg.PGHealthCheckPeriod,
		QueryTimeoutDefault: cfg.PGQueryTimeoutDefault,
	})
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Info("pg pool ready", "max_conns", cfg.PGMaxConns)

	// Which TimescaleDB build this database runs, and whether that is allowed.
	//
	// Always reported, never silently assumed. The default TimescaleDB package
	// is the Community (TSL) build, so getting the Apache one is a deliberate
	// act on every image rebuild — exactly the kind of thing that is quietly
	// lost. Logging it means an operator can see the answer without knowing to
	// go looking.
	//
	// Refusal is opt-in via ATL_REQUIRE_APACHE_TIMESCALE. The Timescale License
	// restricts offering the software as a service, not running it, so the flag
	// belongs to a hosted deployment.
	if edition, derr := pg.DetectTimescaleEdition(ctx, pool); derr != nil {
		log.Warn("could not determine the TimescaleDB edition", "err", derr)
	} else {
		log.Info("timescaledb edition", "edition", string(edition))
		if err := pg.RequireApacheTimescale(edition); err != nil {
			if cfg.RequireApacheTimescale {
				return fmt.Errorf("refusing to start: %w", err)
			}
			if edition == pg.TimescaleCommunity {
				log.Warn("running the Community (TSL) TimescaleDB build. Legal to "+
					"self-host; NOT permitted for a hosted database service, and the "+
					"license's Value Added exception does not cover a product whose "+
					"purpose is letting users modify schema via DDL. Set "+
					"ATL_REQUIRE_APACHE_TIMESCALE=true to make this fatal",
					"edition", string(edition))
			}
		}
	}

	// Whether row-level security means anything at all, checked once at boot.
	//
	// A superuser or BYPASSRLS role leaves every `partition by` policy attached,
	// visible in the catalog and inert: FORCE is set, the catalog agrees, and
	// every read returns every tenant's rows. internal/storage/pg executes that.
	//
	// Refusal is opt-in via ATL_REQUIRE_TENANT_ISOLATION, matching the
	// Timescale check above. A deployment with no partitioned entities is
	// unaffected either way, so defaulting to fatal would break every existing
	// single-tenant install to protect a feature it does not use.
	privs, derr := pg.DetectRolePrivileges(ctx, pool)
	if err := tenantIsolationError(privs, derr, cfg.RequireTenantIsolation); err != nil {
		return err
	}
	switch {
	case derr != nil:
		log.Warn("could not determine whether the database role enforces "+
			"row-level security", "err", derr)
	case !privs.CanEnforceRLS():
		log.Warn("the database role bypasses row-level security, so `partition by` "+
			"provides NO tenant isolation on this deployment — policies are attached "+
			"and inert. Harmless without partitioned entities; a cross-tenant leak "+
			"with them. Set ATL_REQUIRE_TENANT_ISOLATION=true to make this fatal",
			"role", privs.Name, "superuser", privs.Superuser, "bypassrls", privs.BypassRLS)
	default:
		log.Info("database role enforces row-level security", "role", privs.Name)
	}

	obs.RegisterPoolStats(nil, pool.Raw())

	// Auth allowlist is loaded once at startup and refreshed on its own
	// goroutine. The refresher uses a ctx detached from SIGTERM so a
	// blocking reload during shutdown doesn't lock callers out mid-drain.
	authAllowlist := auth.New(pool.Raw(), log.With("component", "auth"))
	if err := authAllowlist.Reload(ctx); err != nil {
		return fmt.Errorf("load auth allowlist: %w", err)
	}
	log.Info("auth allowlist ready", "callers", authAllowlist.Size())
	allowlistCtx, cancelAllowlist := context.WithCancel(context.Background())
	defer cancelAllowlist()
	go authAllowlist.RunRefresher(allowlistCtx, 30*time.Second)

	mc, err := memcached.New(memcached.Config{
		Addrs:        cfg.MemcachedAddrs,
		Timeout:      cfg.MemcachedTimeout,
		MaxIdleConns: 8,
	})
	if err != nil {
		return err
	}
	defer func() { _ = mc.Close() }()
	log.Info("memcached client ready", "addrs", cfg.MemcachedAddrs)

	reader, err := read.New(mc, read.Config{
		LRUSize:       cfg.CacheLRUSize,
		MaxValueBytes: 1 << 20,
		DefaultTTL:    cfg.CacheDefaultTTL,
		XFetchBeta:    cfg.CacheXFetchBeta,
	})
	if err != nil {
		return err
	}

	// One Cache per process. The worker drives BumpGeneration on
	// generation_bump outbox rows; the entity servers (wired below)
	// drive Generation + Lookup + Store on every Query<Entity> RPC.
	queryCache := queryresult.New(mc)

	worker, err := invalidate.NewWorker(pool.Raw(), mc, reader, queryCache, invalidate.WorkerConfig{
		Schema:        "atlantis",
		DrainInterval: cfg.OutboxDrainInterval,
		BatchSize:     cfg.OutboxBatchSize,
		PointerTTL:    cfg.OutboxPointerTTL,
		AlertLag:      cfg.OutboxAlertLag,
		Logger:        log.With("component", "outbox-worker"),
	})
	if err != nil {
		return err
	}

	// Worker uses a ctx detached from SIGTERM so it keeps draining during
	// the gRPC GracefulStop window. cancelWorker fires post-GracefulStop.
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()

	var workerWG sync.WaitGroup
	workerWG.Add(1)
	go func() {
		defer workerWG.Done()
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("outbox worker panic", "panic", rec)
			}
		}()
		if err := worker.Run(workerCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("outbox worker exited", "err", err)
		}
	}()

	log.Debug("init: health http server")
	healthTLSCfg, err := healthTLS(cfg)
	if err != nil {
		return err
	}
	healthHTTP := newHealthServer(cfg.HealthAddr, healthDeps{
		Pool:               pool.Raw(),
		MC:                 mc,
		Worker:             worker,
		WorkerMaxStaleness: 3 * cfg.OutboxDrainInterval,
		ProbeTimeout:       cfg.HealthProbeTimeout,
		StartedAt:          time.Now(),
		Version:            version,
	}, healthTLSCfg, ctx)
	go func() {
		log.Info("health https listening", "addr", cfg.HealthAddr)
		// The certificate and key are already in TLSConfig, so both arguments
		// are empty — ListenAndServeTLS only reads files when they are not.
		if err := healthHTTP.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health https server", "err", err)
		}
	}()

	log.Debug("init: build transport creds")
	creds, err := transportCreds(cfg)
	if err != nil {
		return err
	}

	// Trusted front-proxy mode: nil unless ATL_TRUSTED_PROXY_CALLERS is set.
	// When non-nil, a connection from one of those CNs may forward a
	// re-validated end-client cert that becomes the caller identity.
	fwdAuth, err := newForwardedAuth(cfg)
	if err != nil {
		return err
	}
	if fwdAuth != nil {
		log.Info("trusted front-proxy mode enabled",
			"proxies", cfg.TrustedProxyCallers,
			"header", cfg.TrustedProxyCertHeader,
			"may_apply", cfg.TrustedProxyMayApply,
			"may_operate", cfg.TrustedProxyMayOperate)
	}

	log.Debug("init: build rate-limit interceptor")
	rateLimit := interceptors.NewRateLimit(pool.Raw(), interceptors.RateLimitConfig{
		DefaultQPS:           cfg.RateLimitDefaultQPS,
		Burst:                cfg.RateLimitBurst,
		PerCaller:            cfg.RateLimitPerCaller,
		PoolSaturationCutoff: cfg.RateLimitSaturationCutoff,
		CallerFromContext:    callerFromContext,
	})

	log.Debug("init: grpc.NewServer")
	// One AuthChecker → two interceptors (Unary + Stream) that share the same
	// allowlist + exempt-prefix set.
	//
	// The admin prefix is not exempt. An exemption there bootstraps a caller —
	// applying schema is what writes its caller_registrations row — at the cost
	// of leaving any admin RPC with no authz call of its own reachable by
	// anyone. Registration is an operator act through RegisterCaller, seeded for
	// a fresh install by migration 0019, and the capability interceptor below
	// governs the admin plane.
	//
	// Health and reflection stay exempt: they are infrastructure probes.
	authChecker := interceptors.NewAuthChecker(interceptors.AuthConfig{
		Allowlist: authAllowlist,
		// Unconditional. This used to read `cfg.TLSCertFile != ""`, so a
		// deployment without TLS ran with the allowlist off. loadConfig now
		// requires mTLS, which leaves nothing for the condition to select.
		Enforce:           true,
		CallerFromContext: callerFromContext,
		ExemptPrefixes: []string{
			"/grpc.health.v1.Health/",
			"/grpc.reflection.",
		},
	})

	// Capability enforcement for the admin plane. Grants come from
	// atlantis.caller_capabilities, keyed by the same cert CN the auth,
	// cert-binding and rate-limit layers resolve, and cached on the same 5s TTL
	// as the cert-binding checker so a revocation takes effect at one
	// consistent horizon rather than two.
	//
	// Built before admin.Service because the service reads it too: the job
	// RPCs scope their rows to the calling caller and let an operator see
	// across all of them, and that is one question — "does this identity hold
	// CAPABILITY_OPERATOR" — asked of the same grants the interceptor uses.
	// Two sources for that answer is two things to keep in step.
	adminGrants, err := authz.NewPostgresGrants(authz.PostgresGrantsConfig{
		DB:                authz.Pool(pool.Raw()),
		CallerFromContext: callerFromContext,
		Logger:            log,
	})
	if err != nil {
		return fmt.Errorf("build admin grants: %w", err)
	}

	// admin.Service is constructed early because the cert-binding
	// interceptor needs its LookupCallerCertBinding method. Register on
	// the gRPC server happens after server construction below.
	adminSvc := admin.New(pool.Raw(), admin.Config{
		MirrorDir:          cfg.AdminMirrorDir,
		MirrorEnabled:      cfg.AdminMirrorSchema,
		AllowApplyMutation: cfg.AdminAllowApplyMutation,
		// Share the cert-CN extractor with the auth + rate-limit
		// interceptors so every layer agrees on caller identity for the
		// same request — a divergence here would let a CN authorized
		// by one layer be evaluated as a different identity by another.
		CallerFromContext: callerFromContext,
		BackfillEnabled:   cfg.BackfillWorkerEnabled,
		LogRing:           logRing,

		// The job RPCs ask this one question: may this identity read across
		// callers, or only its own rows. Adapted here so the admin package
		// does not import authz to ask it.
		HasCapability: func(ctx context.Context, c adminpb.Capability) (bool, error) {
			set, err := adminGrants.For(ctx)
			if err != nil {
				return false, err
			}
			return set.Has(c), nil
		},

		// Trusted-proxy admin-plane policy: a forwarded identity may
		// self-apply (default) but not invoke cross-caller operator RPCs
		// unless explicitly opted in. ProxyForwardedFromContext is nil when
		// the mode is off, which disables the gating entirely.
		ProxyForwardedFromContext: proxyForwardedFromContext(fwdAuth),
		TrustedProxyMayApply:      cfg.TrustedProxyMayApply,
		TrustedProxyMayOperate:    cfg.TrustedProxyMayOperate,
	})

	// Cert binding: every authenticated RPC must belong to a caller that still
	// has a caller_identities row. No row — never registered, or revoked — is
	// Unauthenticated, five seconds behind a RevokeCaller.
	//
	// No fingerprint comparison: migration 0032 deleted cert_fingerprint and its
	// two overlap columns. Certificate lifetime is seven days and the trust
	// decision is the chain and the common name, verified at the handshake.
	//
	// Nothing is exempt by default; see CertBindingExemptCallers in config.go.
	// One CertBindingChecker → both interceptor flavors share one
	// TTL cache, so a stream lookup for "vendor" and a unary lookup
	// for "vendor" hit the same cache entry instead of duplicating
	// the DB round-trip across two parallel caches.
	certBindingChecker := interceptors.NewCertBindingChecker(interceptors.CertBindingConfig{
		Lookup:            adminSvc.LookupCallerCertBinding,
		Enforce:           true,
		CallerFromContext: callerFromContext,
		ExemptCallers:     cfg.CertBindingExemptCallers,
		Log:               log,
	})

	// adminGrants is built above, before admin.Service, because the service
	// consults it too. One instance means the interceptor's decision and the
	// job RPCs' scoping read the same grants through the same 5s cache,
	// rather than two caches that can disagree about a revocation.

	unary := []grpc.UnaryServerInterceptor{
		recoveryInterceptor(log),
		interceptors.NewMetrics(),
		resolveCallerInterceptor(fwdAuth),
		certBindingChecker.Unary(),
		authChecker.Unary(),
		// After identity is established, before any handler runs. `partition by`
		// binds this value to the transaction, and a handler that had to read
		// the header itself is a handler a later one can forget to add.
		interceptors.NewPartition(),
	}
	// Capability enforcement goes after identity is resolved and the cert is
	// bound, and before any work happens.
	//
	// Unconditional, like the auth and cert-binding checkers above. All three
	// answer questions about an identity, and only mTLS produces one: without a
	// listener cert the caller name comes from an x-caller header the client
	// writes itself, so a capability check against it would deny whoever is
	// honest and pass whoever types a different string — an appearance of
	// authorization rather than authorization.
	//
	// That is why the three used to switch off together when TLS_CERT_FILE was
	// empty, and why the empty case is now refused at loadConfig instead. A
	// deployment that answers "no" to "is this authenticated" is not a
	// deployment atlantis will start.
	unary = append(unary, adminPolicy.UnaryInterceptor(adminGrants))
	unary = append(unary, rateLimit, loggingInterceptor(log))

	srv := grpc.NewServer(
		grpc.Creds(creds),
		// Match the SDK's 64 MiB client receive default (atltransport):
		// bulk entity Query reads and multi-row batch-procedure writes
		// (e.g. a page of catalog rows) overflow the 4 MiB gRPC default.
		grpc.MaxRecvMsgSize(64<<20),
		grpc.MaxSendMsgSize(64<<20),
		grpc.ChainUnaryInterceptor(unary...),
		// Stream chain mirrors the unary chain order for everything that
		// applies on a per-stream basis. Rate limiting is intentionally
		// excluded — it's an RPCs/sec concept and a long-lived stream
		// (one per worker pod, hours long) doesn't fit. Cert binding +
		// allowlist are reapplied at stream open via the same shared
		// check helpers, so revoked certs and unregistered callers can't
		// bypass via the streaming surface. The capability policy has no
		// stream form because the Admin service declares no streaming RPC —
		// an invariant held by TestAdminServiceDeclaresNoStreamingRPC, since
		// nothing about adding one would fail to compile.
		grpc.ChainStreamInterceptor(
			recoveryStreamInterceptor(log),
			interceptors.NewMetricsStream(),
			resolveCallerStreamInterceptor(fwdAuth),
			certBindingChecker.Stream(),
			authChecker.Stream(),
			interceptors.NewPartitionStream(),
			loggingStreamInterceptor(log),
		),
	)

	log.Debug("init: health + reflection")
	healthSrv := health.NewServer()
	grpc_health_v1.RegisterHealthServer(srv, healthSrv)
	healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	reflection.Register(srv)

	log.Debug("init: register admin service")
	// One service, one wire format. The hand-rolled JSON descriptor that used
	// to be registered alongside this is gone: tide, tidectl and the console
	// all speak protobuf now, so nothing dialled it.
	admin.RegisterGenerated(srv, adminSvc)

	// Backfill worker — gated by ATL_BACKFILL_WORKER_ENABLED. Shares
	// workerCtx with the invalidate worker so SIGTERM stops both, and
	// uses defer-recover so a panic in one row's chunk doesn't take the
	// process down.
	if cfg.BackfillWorkerEnabled {
		bfWorker := backfill.NewWorker(pool.Raw(), backfill.Config{
			Schema:       "atlantis",
			PollInterval: time.Second,
			ChunkSize:    10000,
			Throttle:     100 * time.Millisecond,
			Logger:       log.With("component", "backfill-worker"),
		})
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("backfill worker panic", "panic", rec)
				}
			}()
			if err := bfWorker.Run(workerCtx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("backfill worker exited", "err", err)
			}
		}()
		log.Info("backfill worker enabled")
	}

	// Jobs worker pool — one Worker per queue named in
	// ATL_JOBS_QUEUES. The Registry stays empty in this slice; once
	// caller code uses the generated SDK to RegisterJobHandlers, the
	// worker's runtime lookups will resolve. Until then, submitted
	// jobs sit in atlantis.jobs awaiting handler deployment.
	// Built-in jobs: the maintenance atlantis runs on its own behalf.
	// jobs.RegisterBuiltins registers and schedules in one call, so neither step
	// can be added without the other.
	//
	// Outside the ATL_JOBS_WORKER_ENABLED branch below. That flag governs caller
	// job queues and defaults to false, while destructive migrations retain
	// parked objects for thirty days against something removing them.
	builtinRegistry := jobs.NewRegistry()
	if err := jobs.RegisterBuiltins(ctx, builtinRegistry, pool.Raw(), log); err != nil {
		// Returned, not os.Exit: every defer in run() — the pool, the
		// allowlist refresher, the memcache client, the worker context — is
		// skipped by an exit here, and this would turn a transient database
		// blip during a rolling restart into a hard crash of a process that
		// was otherwise ready to serve.
		return fmt.Errorf("register built-in jobs: %w", err)
	}

	// The scheduler evaluates atlantis.job_schedules and enqueues due work.
	// Every replica runs one; a session-level advisory lock elects the single
	// evaluator, so N pods produce one fire per occurrence rather than N.
	scheduler := &jobs.Scheduler{
		Pool:   pool.Raw(),
		Logger: log.With("component", "jobs-scheduler"),
	}
	workerWG.Add(1)
	go func() {
		defer workerWG.Done()
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("jobs scheduler panic", "panic", rec)
			}
		}()
		if err := scheduler.Run(workerCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("jobs scheduler exited", "err", err)
		}
	}()

	// And a worker on the built-in queue. A schedule firing into an undrained
	// queue does nothing.
	builtinWorker := jobs.NewWorker(pool.Raw(), builtinRegistry, jobs.BuiltinQueue, jobs.Config{
		Schema:        "atlantis",
		DrainInterval: time.Second,
		BatchSize:     10,
		Logger:        log.With("component", "jobs-worker", "queue", jobs.BuiltinQueue),
	})
	workerWG.Add(1)
	go func() {
		defer workerWG.Done()
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("built-in jobs worker panic", "panic", rec)
			}
		}()
		if err := builtinWorker.Run(workerCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("built-in jobs worker exited", "err", err)
		}
	}()

	var jobsRegistry *jobs.Registry
	if cfg.JobsWorkerEnabled {
		jobsRegistry = jobs.NewRegistry()
		for jobName, addr := range cfg.JobsRemoteHandlers {
			jobs.RegisterRemote(jobsRegistry, jobName, addr)
			log.Info("jobs remote handler registered", "job", jobName, "addr", addr)
		}
		for _, queue := range cfg.JobsQueues {
			queue := queue
			w := jobs.NewWorker(pool.Raw(), jobsRegistry, queue, jobs.Config{
				Schema:        "atlantis",
				DrainInterval: time.Second,
				BatchSize:     50,
				Logger:        log.With("component", "jobs-worker", "queue", queue),
			})
			workerWG.Add(1)
			go func() {
				defer workerWG.Done()
				defer func() {
					if rec := recover(); rec != nil {
						log.Error("jobs worker panic", "queue", queue, "panic", rec)
					}
				}()
				if err := w.Run(workerCtx); err != nil && !errors.Is(err, context.Canceled) {
					log.Error("jobs worker exited", "queue", queue, "err", err)
				}
			}()
			log.Info("jobs worker enabled", "queue", queue)
		}
	}
	_ = jobsRegistry // stashed for future public access; today only the in-process worker goroutines need it.

	// Worker-poll dispatcher — gated by ATL_JOBS_DISPATCHER_ENABLED.
	// Drains atlantis.jobs against this pod's PG and pushes work over
	// a bidi gRPC stream to remote workers (laptop-dev or any caller
	// without direct PG access). Coexists safely with direct-PG SDK
	// workers via the shared SKIP LOCKED claim helpers.
	var dispatcher *jobsdispatcher.Dispatcher
	if cfg.JobsDispatcherEnabled {
		dispatcher = jobsdispatcher.New(pool.Raw(), jobsdispatcher.Config{
			// 5-minute default is the Temporal-aligned lease budget
			// for long-running handlers. Short-running handlers can
			// override down via the DSL `heartbeat 30s` modifier;
			// long-running ones override up via `heartbeat 10m`.
			// (Auto-heartbeat tick is HeartbeatBudget/3, sent to the
			// SDK via SessionAccepted at session open.)
			HeartbeatBudget: 5 * time.Minute,
			DrainInterval:   time.Second,
			BatchSize:       50,
			AckTimeoutMS:    150000, // HeartbeatBudget / 2
			ShutdownBudget:  30 * time.Second,
			PodID:           podID(),
			IRLoader: func(_ context.Context) (*dsl.IR, error) {
				ir, _, err := loadIRCheckpoint(pool)
				return ir, err
			},
			// Alias loader lets the dispatcher accept a caller whose CN
			// satisfies visible_to via an operator-configured alias
			// (PostgreSQL-roles / AD-SID / DNS-CNAME pattern). Fetched
			// once per stream at Open; cached on the session for its
			// lifetime so steady-state workers never re-fetch.
			AliasLoader:       adminSvc.LookupCallerAliases,
			CallerFromContext: callerFromContext,
			Logger:            log.With("component", "jobs-dispatcher"),
		})
		jobsdispatcher.Register(srv, dispatcher)
		adminSvc.SetDispatcher(newDispatcherAdapter(dispatcher))
		for _, queue := range cfg.JobsDispatcherQueues {
			queue := queue
			workerWG.Add(1)
			go func() {
				defer workerWG.Done()
				defer func() {
					if rec := recover(); rec != nil {
						log.Error("jobs dispatcher panic", "queue", queue, "panic", rec)
					}
				}()
				dispatcher.RunQueue(workerCtx, queue)
			}()
			log.Info("jobs dispatcher enabled", "queue", queue)
		}
	} else {
		log.Info("jobs dispatcher disabled (set ATL_JOBS_DISPATCHER_ENABLED=true to enable)")
	}

	log.Debug("init: load IR checkpoint")
	ir, irHash, err := loadIRCheckpoint(pool)
	if err != nil {
		return fmt.Errorf("load IR checkpoint: %w", err)
	}
	if irHash != "" {
		log.Info("loaded IR checkpoint", "hash", irHash[:min(12, len(irHash))], "entities", len(ir.Entities))
	}

	// Audit the checkpoint for SQL that would defeat tenant isolation.
	//
	// The validator that rejects set_config and set_partition runs at plan and
	// apply, so it is prospective only: anything stored before it existed has
	// never been looked at and executes with the same authority as anything
	// else. This is also the only place the deploy order can be enforced.
	// Migration 0024 makes the tenant discriminator a PGC_USERSET parameter,
	// and a binary predating the gate would happily serve a checkpoint
	// containing a call the gate now refuses — nothing can stop that binary
	// running, but this one can decline to serve what it let in.
	auditFindings := sqlvalidate.AuditForbiddenCalls(ir)
	if err := storedSQLAuditError(auditFindings, cfg.RequireTenantIsolation); err != nil {
		return err
	}

	// Ask the database whether every partitioned entity's table actually
	// carries an enforced policy.
	//
	// diffPartition compares two declarations; this compares a declaration
	// against the live catalogue. A correct declaration sits over a table with
	// no policy when the migration was planned and never applied, when the
	// database was adopted with the clause already written, or when the policy
	// was dropped out of band.
	//
	// Binding a tenant succeeds whether or not a policy exists, so the
	// observable signal — omit the tenant, get refused — reports healthy either
	// way while every read returns every tenant's rows.
	//
	// One closure, used at boot and on every hot reload. A checkpoint reaches a
	// running server through LISTEN/NOTIFY, and the reload turns on enforcement
	// in the dispatcher for whatever the table carries then.
	//
	// It returns the table count with the findings, so the gate is not handed a
	// separately computed one. Two walks over the same IR let
	// `len(partitionedEntities(ir))` be replaced by 0 at both call sites, which
	// turns the probe-failure refusal off with the suite green.
	verifyPartitions := func(ctx context.Context, ir *dsl.IR) ([]string, int, error) {
		var tables []pg.PartitionedTable
		for i := range ir.Entities {
			e := &ir.Entities[i]
			if e.PartitionField == "" {
				continue
			}
			tables = append(tables, pg.PartitionedTable{
				EntityID: e.ID(),
				Schema:   schema.EntitySchema(e),
				Table:    schema.EntityPhysicalTable(e),
				Column:   e.PartitionField,
			})
		}
		problems, err := pg.VerifyPartitionPolicies(ctx, pool, tables)
		return problems, len(tables), err
	}

	policyProblems, partitionedTables, perr := verifyPartitions(ctx, ir)
	if perr != nil {
		log.Warn("could not verify that partitioned entities carry a policy", "err", perr)
	}
	if err := partitionGate(policyProblems, perr, cfg.RequireTenantIsolation,
		partitionedTables); err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}
	for _, p := range policyProblems {
		log.Warn("a partitioned entity has no enforced row-level security; reads "+
			"return every tenant's rows. Set ATL_REQUIRE_TENANT_ISOLATION=true to "+
			"make this fatal", "problem", p)
	}
	if len(auditFindings) > 0 {
		for _, f := range auditFindings {
			log.Warn("stored SQL can rebind the caller's tenant, so `partition by` "+
				"provides no isolation against it. Remove it and re-apply; set "+
				"ATL_REQUIRE_TENANT_ISOLATION=true to make this fatal", "finding", f.Error())
		}
	}

	log.Debug("init: register entity services")
	dynServer := entity.NewServer(pool, mc, invalidate.NewOutbox(), queryCache, reader)
	if err := dynServer.Register(srv, ir); err != nil {
		return fmt.Errorf("register entity services: %w", err)
	}

	// The same verification on every hot reload, refusing a schema that would
	// turn on enforcement the database is not providing.
	//
	// Refuses only when ATL_REQUIRE_TENANT_ISOLATION is set, matching boot: a
	// deployment that tolerates the warning at startup should not have a reload
	// fail on it, and a reload that fails leaves the OLD schema serving, which
	// is its own surprise.
	dynServer.SetOnReload(func(newIR *dsl.IR) error {
		problems, partitionedTables, err := verifyPartitions(context.WithoutCancel(ctx), newIR)
		if err != nil {
			log.Warn("could not verify partitioned entities on reload", "err", err)
		}
		for _, p := range problems {
			log.Warn("a reloaded schema declares `partition by` on a table with no "+
				"enforced row-level security", "problem", p)
		}
		// The SAME decision as boot, from the same function. These were two
		// inline copies of one rule, and a review turned each off separately.
		if gateErr := partitionGate(problems, err, cfg.RequireTenantIsolation,
			partitionedTables); gateErr != nil {
			// Not "refusing to start" — this server is already running. A
			// failed reload leaves the PREVIOUS schema serving.
			return fmt.Errorf("refusing to serve the reloaded schema: %w", gateErr)
		}
		return nil
	})

	log.Debug("init: schema listener")
	schemaListener := entity.NewSchemaListener(pool.Raw(), dynServer, func(ctx context.Context) (*dsl.IR, string, error) {
		return loadIRCheckpoint(pool)
	}, log.With("component", "schema-listener"))
	workerWG.Add(1)
	go func() {
		defer workerWG.Done()
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("schema listener panic", "panic", rec)
			}
		}()
		if err := schemaListener.Run(workerCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("schema listener exited", "err", err)
		}
	}()
	log.Info("schema listener enabled")

	log.Debug("init: net.Listen", "addr", cfg.GRPCAddr)
	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return err
	}
	defer func() { _ = lis.Close() }()
	log.Info("grpc listening", "addr", cfg.GRPCAddr)

	// Serve until ctx is canceled; then GracefulStop.
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(lis); err != nil {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		return err
	}

	// Graceful shutdown: stop accepting new RPCs, wait for in-flight up
	// to a bounded budget, then force.
	healthSrv.Shutdown()
	shutdownDone := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(15 * time.Second):
		log.Warn("graceful shutdown timed out; forcing")
		srv.Stop()
	}

	// Dispatcher drain runs AFTER gRPC GracefulStop (so no new
	// WorkerSession streams arrive) and BEFORE pool.Close() (because
	// the drain writes release rows to PG). Sessions get Goodbye'd,
	// in-flight rows wait for the configured budget, anything
	// remaining is force-released back to pending.
	if dispatcher != nil {
		dispatchShutdownCtx, cancelDispatch := context.WithTimeout(context.Background(), 45*time.Second)
		released := dispatcher.Shutdown(dispatchShutdownCtx)
		cancelDispatch()
		if released > 0 {
			log.Warn("dispatcher shutdown force-released rows", "count", released)
		}
	}

	// Final drain pass: stop the worker loop, wait for the goroutine,
	// then flush rows that in-flight RPCs enqueued during GracefulStop.
	// Otherwise a pod restart leaves pending invalidations for the next
	// pod and readers see stale cache in the gap.
	cancelWorker()
	workerWG.Wait()

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDrain()
	if err := worker.Drain(drainCtx); err != nil {
		log.Warn("final outbox drain incomplete", "err", err)
	}

	healthShutdownCtx, cancelHealthShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelHealthShutdown()
	if err := healthHTTP.Shutdown(healthShutdownCtx); err != nil {
		log.Warn("health http shutdown", "err", err)
	}
	return nil
}

// buildLogger wires slog from the LOG_LEVEL env var. JSON in non-dev, text
// when LOG_LEVEL is "debug" so local runs are readable.
//
// The returned LogRing is teed off the same handler — every slog call
// also publishes into it for the console's Health page tail. The ring
// is lock-free (see internal/obs/logring.go) so the tee adds ~50-100 ns
// per emit, invisible at millisecond-scale RPC latencies.
func buildLogger(cfg config) (*slog.Logger, *obs.LogRing) {
	lvl := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	var base slog.Handler
	if cfg.LogLevel == "debug" {
		base = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	} else {
		base = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	}
	ring := obs.NewLogRing(cfg.LogRingSize)
	h := obs.NewRingHandler(base, ring)
	log := slog.New(h)
	slog.SetDefault(log)
	return log, ring
}

// loadIRCheckpoint reads the current IR and content hash from
// atlantis.ir_checkpoint. If no checkpoint exists yet (fresh database),
// an empty IR is returned so the server can start and accept PlanSchema.
func loadIRCheckpoint(pool *pg.Pool) (*dsl.IR, string, error) {
	var raw []byte
	var contentHash *string
	err := pool.QueryRow(context.Background(),
		`SELECT ir, content_hash FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&raw, &contentHash)
	if err != nil {
		return &dsl.IR{Version: dsl.CurrentIRVersion}, "", nil
	}
	ir, err := dsl.DecodeJSONIR(raw)
	if err != nil {
		return nil, "", err
	}
	hash := ""
	if contentHash != nil {
		hash = *contentHash
	}
	return ir, hash, nil
}
