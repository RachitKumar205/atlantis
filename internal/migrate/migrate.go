// Package migrate applies the SQL migration tree to a Postgres database.
//
// It lives here rather than in cmd/server because several things need it and
// only one of them is the server: the console migrates its own schema, and
// tests that boot against a private database have to build the schema first —
// and a test binary cannot call into package main.
//
// # Where a migration tree comes from
//
// Two sources, split by who owns the schema rather than by convenience.
//
// A tree the PRODUCT owns is embedded in the binary that needs it
// ([RunFS]): migrations/infra in atlantis-server, migrations/console in
// atlantis-console. The binary then carries the schema it was built against,
// so there is no path to configure and no way to run one version of a binary
// against another version of its own schema.
//
// A tree the DEPLOYMENT owns stays on disk ([RunDir]). tidectl emits those
// into the deployment's own repository after the binary is built, so no binary
// can embed them.
package migrate

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// History table names. Each tree rides its own so they evolve independently,
// and so a version recorded by one is never mistaken for another's.
const (
	InfraHistoryTable   = "atlantis_schema_migrations_infra"
	TidectlHistoryTable = "atlantis_schema_migrations_tidectl"
	ConsoleHistoryTable = "console_schema_migrations"
)

// Run applies the server's pending migrations: the embedded infra tree, then
// the tidectl tree from disk.
//
// infra goes first because tidectl-emitted trigger functions reference the
// cache_invalidations table infra creates. A no-op (already-current) leg logs
// at info and continues.
//
// Failures here are fatal: starting the server against an out-of-date schema
// would let RPCs hit columns that don't exist yet. We'd rather crash on boot
// than serve garbage.
//
// infraFS is the embedded tree — see the package doc for why the product's own
// schema travels in the binary while the deployment's stays on disk.
// tidectlDir may be missing or empty, which is a legitimate state: a fresh
// install with no callers has no tidectl migrations yet.
func Run(pgURL string, infraFS fs.FS, tidectlDir string, log *slog.Logger) error {
	if err := RunFS(pgURL, infraFS, "infra", InfraHistoryTable, log); err != nil {
		return err
	}
	return RunDir(pgURL, tidectlDir, "tidectl", TidectlHistoryTable, log)
}

// RunFS applies an embedded migration tree. sub names the directory inside fsys
// and is used only for log lines and error messages.
func RunFS(pgURL string, fsys fs.FS, sub, historyTable string, log *slog.Logger) error {
	// An embedded tree with no .sql files is a build mistake, not a state:
	// //go:embed fails at compile time on a missing directory, so reaching here
	// with nothing in it means the pattern matched a directory that exists and
	// is empty. Say so rather than logging "skipped" and starting against a
	// database with no schema.
	entries, err := fs.ReadDir(fsys, sub)
	if err != nil {
		return fmt.Errorf("migrate %s: read embedded tree: %w", sub, err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("migrate %s: the embedded tree is empty, so this "+
			"binary carries no schema for it", sub)
	}

	src, err := iofs.New(fsys, sub)
	if err != nil {
		return fmt.Errorf("migrate %s: open embedded tree: %w", sub, err)
	}
	return apply(pgURL, src, "iofs", sub, historyTable, log)
}

// RunDir applies a migration tree from the filesystem.
//
// A missing or empty directory is a legitimate state here — unlike RunFS,
// nothing guarantees the deployment has emitted any migrations yet — so it logs
// and returns nil.
func RunDir(pgURL, root, sub, historyTable string, log *slog.Logger) error {
	dir := filepath.Join(root, sub)
	if entries, err := os.ReadDir(dir); err != nil || len(entries) == 0 {
		log.Info("auto-migrate skipped (no migrations)", "dir", sub)
		return nil
	}
	src, err := source.Open("file://" + dir)
	if err != nil {
		return fmt.Errorf("migrate %s: open %s: %w", sub, dir, err)
	}
	return apply(pgURL, src, "file", sub, historyTable, log)
}

// apply runs `migrate up` from one source against its private history table.
// The version number reported in logs is per-tree; an operator reading a server
// boot sees two version stamps, one per history.
//
// srcName is the driver name golang-migrate registers the source under —
// "iofs" or "file". It affects nothing but its own error messages.
func apply(pgURL string, src source.Driver, srcName, sub, historyTable string, log *slog.Logger) error {
	sep := "?"
	if strings.Contains(pgURL, "?") {
		sep = "&"
	}
	// search_path is pinned, and without it this re-runs the whole history on
	// every boot after the first.
	//
	// golang-migrate names its version table unqualified, so where it lands is
	// whatever search_path resolves to. The default is `"$user", public`, the
	// role is called `atlantis`, and migration 1 creates a schema also called
	// `atlantis` — so:
	//
	//   - First run: no `atlantis` schema yet, `"$user"` resolves to nothing,
	//     the table is created in `public` and the run records version 27 there.
	//   - Every later connection: the `atlantis` schema now exists, `"$user"`
	//     resolves to it, and the unqualified name finds no table. golang-migrate
	//     creates an empty one and concludes nothing has ever been applied.
	//
	// The consequences are not subtle once seen. `migrate version` reports "no
	// migration" against a fully migrated database. `down` and `down -all` roll
	// nothing back, which is why the reversibility gate in the Makefile passed
	// while verifying nothing. And a second `up` replays every migration from
	// zero — survivable only because most are `CREATE ... IF NOT EXISTS`, which
	// is luck rather than design.
	//
	// `public` rather than a qualified table name: every deployment that exists
	// already has its history there, having been created by a first run. Moving
	// to `atlantis.<table>` would strand that history and re-run everything
	// once, which is the failure being fixed.
	dbURL := "pgx5://" + trimScheme(pgURL) + sep +
		"search_path=public&x-migrations-table=" + historyTable

	m, err := migrate.NewWithSourceInstance(srcName, src, dbURL)
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
