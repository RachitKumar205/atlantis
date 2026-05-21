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
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/rachitkumar205/atlantis/internal/cache/invalidate"
	"github.com/rachitkumar205/atlantis/internal/cache/memcached"
	"github.com/rachitkumar205/atlantis/internal/cache/queryresult"
	"github.com/rachitkumar205/atlantis/internal/cache/read"
	"github.com/rachitkumar205/atlantis/internal/server/admin"
	"github.com/rachitkumar205/atlantis/internal/server/interceptors"
	"github.com/rachitkumar205/atlantis/internal/storage/pg"

	entityserver "github.com/rachitkumar205/atlantis/gen/go/server"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		// Use stderr directly — the logger isn't built yet.
		os.Stderr.WriteString("config: " + err.Error() + "\n")
		os.Exit(2)
	}

	log := buildLogger(cfg)

	// Top-level context cancels on SIGINT / SIGTERM.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

// run does the actual work; separate from main so tests can drive it. Each
// resource is created and Close-deferred in order — pool first so the
// invalidation worker (which acquires a connection) can register cleanup
// before its Run starts.
func run(ctx context.Context, cfg config, log *slog.Logger) error {
	if cfg.AutoMigrate {
		if err := runAutoMigrate(cfg.PGURL, cfg.MigrationsDir, log); err != nil {
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

	mc, err := memcached.New(memcached.Config{
		Addrs:        cfg.MemcachedAddrs,
		Timeout:      cfg.MemcachedTimeout,
		MaxIdleConns: 8,
	})
	if err != nil {
		return err
	}
	defer mc.Close()
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

	var workerWG sync.WaitGroup
	workerWG.Add(1)
	go func() {
		defer workerWG.Done()
		if err := worker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("outbox worker exited", "err", err)
		}
	}()

	log.Debug("init: build transport creds")
	creds, err := transportCreds(cfg, log)
	if err != nil {
		return err
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
	srv := grpc.NewServer(
		grpc.Creds(creds),
		grpc.ChainUnaryInterceptor(
			recoveryInterceptor(log),
			resolveCallerInterceptor(),
			rateLimit,
			loggingInterceptor(log),
		),
	)

	log.Debug("init: health + reflection")
	healthSrv := health.NewServer()
	grpc_health_v1.RegisterHealthServer(srv, healthSrv)
	healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	reflection.Register(srv)

	log.Debug("init: register admin service")
	admin.Register(srv, admin.New(pool.Raw(), admin.Config{
		MirrorDir:          cfg.AdminMirrorDir,
		MirrorEnabled:      cfg.AdminMirrorSchema,
		AllowApplyMutation: cfg.AdminAllowApplyMutation,
	}))

	log.Debug("init: register entity services")
	entityserver.Register(srv, entityserver.ServerDeps{
		Pool:       pool,
		Cache:      mc,
		Outbox:     invalidate.NewOutbox(),
		QueryCache: queryCache,
	})

	log.Debug("init: net.Listen", "addr", cfg.GRPCAddr)
	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return err
	}
	defer lis.Close()
	log.Info("grtide listening", "addr", cfg.GRPCAddr)

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
	workerWG.Wait()
	return nil
}

// buildLogger wires slog from the LOG_LEVEL env var. JSON in non-dev, text
// when LOG_LEVEL is "debug" so local runs are readable.
func buildLogger(cfg config) *slog.Logger {
	lvl := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	var h slog.Handler
	if cfg.LogLevel == "debug" {
		h = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	} else {
		h = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	}
	log := slog.New(h)
	slog.SetDefault(log)
	return log
}
