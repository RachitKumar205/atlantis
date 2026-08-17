package pgcatalog

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// PrivateDatabase creates an empty database called name, installs the three
// extensions the migration tree references, and returns a DSN addressing it.
// It is dropped when the test ends.
//
// # Why tests want a private database rather than the shared one
//
// Anything that drives a real apply writes atlantis.ir_checkpoint, which is one
// row for the whole deployment. Run against the developer's database, the
// checkpoint is left holding test entities and every later `tide plan` proposes
// dropping them.
//
// # Why this is a helper and not four copies
//
// It was four copies, and each one silently reused a stale database. They all
// terminated the other backends and dropped in two separate statements —
// pg_terminate_backend signals a backend, it does not wait for it to exit, so
// under load the DROP that follows still saw connections and failed with
// SQLSTATE 55006. Three of the four routed that through [Exec], which logs
// rather than fails, so the CREATE that followed hit "already exists", also
// logged, and the test then ran against the PREVIOUS test's database. What the
// developer saw was a parse error naming a duplicate entity — two layers away
// from the connection that had not finished draining.
//
// DROP DATABASE ... WITH (FORCE) is PostgreSQL doing the terminate and the drop
// as one operation, which is the whole race. It has been available since
// PostgreSQL 13; the pinned image is 17.
func PrivateDatabase(t *testing.T, adminDSN, name string) string {
	t.Helper()

	// Derived first. A DSN this helper cannot rewrite is a fixture mistake, and
	// discovering it before anything is created leaves no database behind.
	dsn := swapDatabase(t, adminDSN, name)

	drop := func(t *testing.T) {
		t.Helper()
		Do(t, adminDSN, func(conn *pgx.Conn) error {
			return dropDatabase(conn, name)
		})
	}
	drop(t)
	// Registered before any pool the caller opens, so it runs after that pool's
	// Close: t.Cleanup is LIFO. FORCE means the order no longer decides whether
	// the drop succeeds, but a drop racing a live pool still logs terminated
	// connections into the caller's output for no reason.
	t.Cleanup(func() { drop(t) })

	Do(t, adminDSN, func(conn *pgx.Conn) error {
		_, err := conn.Exec(context.Background(),
			`CREATE DATABASE `+pgx.Identifier{name}.Sanitize())
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		return nil
	})

	// Extensions are per-database. A fresh one has none, and a pool opened on it
	// fails with `register pgvector: vector type not found` long before any
	// migration runs.
	Do(t, dsn, func(conn *pgx.Conn) error {
		for _, ext := range []string{"citext", "vector", "timescaledb"} {
			if _, err := conn.Exec(context.Background(),
				`CREATE EXTENSION IF NOT EXISTS `+pgx.Identifier{ext}.Sanitize()); err != nil {
				return fmt.Errorf("create extension %s on %s: %w", ext, name, err)
			}
		}
		return nil
	})

	return dsn
}

// dropDatabase removes name, waiting out the cases FORCE does not cover.
//
// FORCE terminates ordinary backends, but it declines when the database holds a
// prepared transaction or a logical replication slot, and it cannot stop a
// connection opened after it took its snapshot. Neither is expected here, so the
// retry is a backstop with a deadline rather than a routine path — and when the
// deadline passes the error is returned, because a database left behind is
// inherited by the next run.
func dropDatabase(conn *pgx.Conn, name string) error {
	ctx := context.Background()
	sql := `DROP DATABASE IF EXISTS ` + pgx.Identifier{name}.Sanitize() + ` WITH (FORCE)`

	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := conn.Exec(ctx, sql)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("could not drop %s, so it is left behind and the "+
				"next run inherits it: %w", name, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// swapDatabase rewrites the database component of a DSN, keeping credentials
// and options.
func swapDatabase(t *testing.T, adminDSN, name string) string {
	t.Helper()
	const marker = "/atlantis?"
	i := strings.LastIndex(adminDSN, marker)
	if i < 0 {
		t.Fatalf("cannot derive a DSN for %s from %q — expected it to end in %s...",
			name, adminDSN, marker)
	}
	return adminDSN[:i] + "/" + name + adminDSN[i+len("/atlantis"):]
}
