// Package migrate applies the SQL migration tree to a Postgres database.
//
// Not in cmd/server: the console migrates its own schema, and a test that boots
// against a private database builds the schema first. A test binary cannot call
// into package main.
//
// A migration tree comes from one of two sources, split by which side owns the
// schema.
//
// A tree the product owns is embedded in the binary that needs it ([RunFS]):
// migrations/infra in atlantis-server, migrations/console in atlantis-console.
// The binary carries the schema it was built against, so there is no path to
// configure and no way to run a binary against another version of its own
// schema.
//
// A tree the deployment owns stays on disk ([RunDir]). tidectl emits those into
// the deployment's repository after the binary is built, so no binary can embed
// them.
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
	CloudHistoryTable   = "cloud_schema_migrations"
)

// Run applies the server's pending migrations: the embedded infra tree, then
// the tidectl tree from disk.
//
// infra goes first: tidectl-emitted trigger functions reference the
// cache_invalidations table infra creates. An already-current leg logs at info
// and continues.
//
// An error here is fatal to boot. An out-of-date schema leaves RPCs reading
// columns that do not exist yet.
//
// tidectlDir may be missing or empty. A fresh install with no callers has no
// tidectl migrations.
func Run(pgURL string, infraFS fs.FS, tidectlDir string, log *slog.Logger) error {
	if err := RunFS(pgURL, infraFS, "infra", InfraHistoryTable, log); err != nil {
		return err
	}
	return RunDir(pgURL, tidectlDir, "tidectl", TidectlHistoryTable, log)
}

// RunFS applies an embedded migration tree. sub names the directory inside fsys
// and is used only for log lines and error messages.
func RunFS(pgURL string, fsys fs.FS, sub, historyTable string, log *slog.Logger) error {
	// An empty embedded tree is a build mistake, not a state. //go:embed fails
	// at compile time on a missing directory, so reaching here with nothing in
	// it means the pattern matched an empty one, and logging "skipped" would
	// start the binary against a database with no schema.
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
// A missing or empty directory logs and returns nil. Nothing guarantees the
// deployment has emitted any migrations yet.
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
// The logged version is per-tree, so a server boot logs two version stamps.
//
// srcName is the driver name golang-migrate registers the source under, "iofs"
// or "file". It reaches nothing but that package's error messages.
func apply(pgURL string, src source.Driver, srcName, sub, historyTable string, log *slog.Logger) error {
	sep := "?"
	if strings.Contains(pgURL, "?") {
		sep = "&"
	}
	// search_path is pinned. Without it, this re-runs the whole history on every
	// boot after the first.
	//
	// golang-migrate names its version table unqualified, so it lands wherever
	// search_path resolves. The default is `"$user", public`, the role is
	// `atlantis`, and migration 0000 creates a schema also called `atlantis`:
	//
	//   - First run: no `atlantis` schema yet, `"$user"` resolves to nothing,
	//     the table is created in `public` and the version is recorded there.
	//   - Every later connection: the `atlantis` schema exists, `"$user"`
	//     resolves to it, and the unqualified name finds no table.
	//     golang-migrate creates an empty one and reports nothing applied.
	//
	// `migrate version` then reports "no migration" against a fully migrated
	// database, `down` and `down -all` roll nothing back — which the
	// migrate-roundtrip target in the Makefile reads as success — and a second
	// `up` replays every migration from zero, surviving only on
	// `CREATE ... IF NOT EXISTS`.
	//
	// `public` rather than a qualified name: every existing deployment already
	// has its history there from a first run, and `atlantis.<table>` would
	// strand it and re-run everything once.
	dbURL := "pgx5://" + trimScheme(pgURL) + sep +
		"search_path=public&x-migrations-table=" + historyTable

	m, err := migrate.NewWithSourceInstance(srcName, src, dbURL)
	if err != nil {
		return fmt.Errorf("migrate init %s: %w", sub, err)
	}
	defer func() {
		// migrate.New opens its own connection. A close error is discarded;
		// the caller continues on its own pool.
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

// trimScheme strips the postgres:// prefix so pgx5:// can be prepended without
// doubling the scheme. The golang-migrate pgx driver registers itself as `pgx5`
// whatever scheme the original URL used.
func trimScheme(url string) string {
	for _, prefix := range []string{"postgres://", "postgresql://"} {
		if len(url) >= len(prefix) && url[:len(prefix)] == prefix {
			return url[len(prefix):]
		}
	}
	return url
}
