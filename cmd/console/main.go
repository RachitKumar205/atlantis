package main

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rachitkumar205/atlantis/internal/console"
)

//go:embed dist
var distFS embed.FS

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := console.ConfigFromEnv()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	// Serve dist/ sub-tree so paths are rooted at "/" not "dist/".
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		log.Error("embed dist", "err", err)
		os.Exit(1)
	}

	srv, err := console.New(cfg, sub, log)
	if err != nil {
		log.Error("init console", "err", err)
		os.Exit(1)
	}
	defer srv.Close()

	httpSrv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      srv,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("atlantis-console listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http listen", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
}
