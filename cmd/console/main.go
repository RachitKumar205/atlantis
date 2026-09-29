package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rachitkumar205/atlantis/internal/console"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := console.ConfigFromEnv()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	// spa_embed.go or spa_none.go, depending on the embedspa build tag.
	sub, err := spaFS()
	if err != nil {
		log.Error("embed dist", "err", err)
		os.Exit(1)
	}
	log.Info("starting", "spa_embedded", sub != nil)

	srv, err := console.New(cfg, sub, log)
	if err != nil {
		log.Error("init console", "err", err)
		os.Exit(1)
	}
	defer srv.Close()

	httpSrv := &http.Server{
		Addr:        cfg.Listen,
		Handler:     srv,
		ReadTimeout: 15 * time.Second,

		// Long enough for the slowest thing this console proxies: reading a
		// schema out of a database it does not run. An organisation that
		// adopted an existing database is introspected across the internet,
		// and 621 tables took longer than the 30s this used to allow — the
		// browser saw a closed connection and no error.
		//
		// It bounds a stalled write, not the work: a handler that takes this
		// long is one to make asynchronous rather than one to wait longer for.
		WriteTimeout: 5 * time.Minute,
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

	// The enrolment listener, when enrolment is configured.
	//
	// Separate from the one above because that one serves browsers behind a TLS
	// terminator this process does not control, and cannot inspect a client
	// certificate. This one terminates its own TLS. It carries the enrolment
	// routes and nothing else — see internal/console/enroll.go.
	//
	// A failure here stops the process rather than leaving the console serving
	// with enrolment quietly absent, which is the shape the predecessor of this
	// feature had for its entire life.
	go func() {
		if err := srv.ServeEnrollment(); err != nil {
			log.Error("enrolment listen", "err", err)
			stop()
		}
	}()

	// The metrics listener, on its own loopback address.
	//
	// A failure here is logged and the console keeps serving, unlike the two
	// above. Metrics absent costs the fleet view; stopping a working console
	// over it trades the job for the reporting on the job.
	go func() {
		if err := srv.ServeMetrics(); err != nil {
			log.Error("metrics listen", "err", err)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
	_ = srv.ShutdownEnrollment(shutCtx)
	_ = srv.ShutdownMetrics(shutCtx)
}
