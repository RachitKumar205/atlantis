package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

// runAutoMigrate applies any pending migrations against the configured
// Postgres URL when AUTO_MIGRATE is enabled (the default in
// docker-compose / make dev-isolated; off in production where ops runs
// migrations explicitly).
//
// The migrations tree is split into infra/ (hand-written) and tidectl/
// (codegen-emitted); each rides its own _schema_migrations history
// table so the two evolve independently. We always apply infra first
// because tidectl-emitted trigger functions reference the
// cache_invalidations table that infra creates. A no-op
// (already-current) leg logs at info and continues.
//
// Failures here are fatal: starting the server against an out-of-date
// schema would let RPCs hit columns that don't exist yet. We'd rather
// crash on boot than serve garbage.
func runAutoMigrate(pgURL string, migrationsDir string, log *slog.Logger) error {
	if err := applyDir(pgURL, migrationsDir, "infra", "atlantis_schema_migrations_infra", log); err != nil {
		return err
	}
	return applyDir(pgURL, migrationsDir, "tidectl", "atlantis_schema_migrations_tidectl", log)
}

// applyDir runs `migrate up` against one subdirectory + its private
// history table. The version number reported in logs is per-subdir;
// operators reading the log see two version stamps per boot, one per
// history.
func applyDir(pgURL, root, sub, historyTable string, log *slog.Logger) error {
	// A missing or empty subdir is a legitimate state — a fresh install
	// with no callers has no tidectl/ migrations yet, and the auto-migrate
	// shouldn't fail on it. golang-migrate errors out of migrate.New if
	// the source path doesn't exist, so guard explicitly.
	dir := filepath.Join(root, sub)
	if entries, err := os.ReadDir(dir); err != nil || len(entries) == 0 {
		log.Info("auto-migrate skipped (no migrations)", "dir", sub)
		return nil
	}

	src := "file://" + dir
	sep := "?"
	if strings.Contains(pgURL, "?") {
		sep = "&"
	}
	dbURL := "pgx5://" + trimScheme(pgURL) + sep + "x-migrations-table=" + historyTable

	m, err := migrate.New(src, dbURL)
	if err != nil {
		return fmt.Errorf("migrate init %s: %w", sub, err)
	}
	defer func() {
		// migrate.New opens its own DB connection; close it explicitly so
		// we don't leak. Errors here are non-fatal (process is about to
		// continue and use its own pool).
		_, _ = m.Close()
	}()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up %s: %w", sub, err)
	}
	v, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("migrate version %s: %w", sub, err)
	}
	log.Info("auto-migrate complete", "dir", sub, "version", v, "dirty", dirty)
	return nil
}

// trimScheme strips the postgres:// prefix so we can prepend pgx5://
// without doubling up the scheme. The golang-migrate pgx driver registers
// itself as `pgx5` regardless of which scheme the original URL used.
func trimScheme(url string) string {
	for _, prefix := range []string{"postgres://", "postgresql://"} {
		if len(url) >= len(prefix) && url[:len(prefix)] == prefix {
			return url[len(prefix):]
		}
	}
	return url
}
