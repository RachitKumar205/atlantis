package migrate_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/migrate"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
	"github.com/rachitkumar205/atlantis/migrations"
)

// migrations/console carries a schema that every running console already built
// for itself, by re-executing one idempotent CREATE-IF-NOT-EXISTS block on each
// boot. The tests below pin what the migration tree owes those databases.
//
// Env-gated like the other live-PG tests:
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable \
//	  go test ./internal/migrate/

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func adminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the migration runner")
	}
	return dsn
}

// theOldWay is the schema the console built for itself before it had
// migrations, and what migration 0001 has to reproduce.
//
// A literal, since nothing in the tree emits it any more. Where the baseline
// drifts from this, a database built by an old console differs from one built
// today, and the difference surfaces at the next migration.
const theOldWay = `
CREATE SCHEMA IF NOT EXISTS console;
CREATE TABLE IF NOT EXISTS console.users (
	id            BIGSERIAL PRIMARY KEY,
	email         TEXT        NOT NULL UNIQUE,
	password_hash TEXT        NOT NULL,
	role          TEXT        NOT NULL DEFAULT 'admin',
	first_name    TEXT        NOT NULL DEFAULT '',
	last_name     TEXT        NOT NULL DEFAULT '',
	created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS console.sessions (
	token      TEXT        PRIMARY KEY,
	user_id    BIGINT      NOT NULL REFERENCES console.users(id) ON DELETE CASCADE,
	expires_at TIMESTAMPTZ NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	sudo_until TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS console_sessions_user_id_idx ON console.sessions(user_id);
CREATE INDEX IF NOT EXISTS console_sessions_expires_idx ON console.sessions(expires_at);
CREATE TABLE IF NOT EXISTS console.audit_log (
	id         BIGSERIAL,
	user_id    BIGINT      NOT NULL,
	action     TEXT        NOT NULL,
	detail     JSONB,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
CREATE INDEX IF NOT EXISTS console_audit_log_user_id_idx ON console.audit_log(user_id);
CREATE INDEX IF NOT EXISTS console_audit_log_created_at_idx ON console.audit_log(created_at DESC);
`

// A database built the old way converges, keeping its rows.
//
// Every console that has ever run carries these tables, so migration 0001 is a
// no-op against them and records its version. Without the second, the next
// migration to add a column runs against a database golang-migrate reads as
// empty.
func TestAnExistingConsoleDatabaseConverges(t *testing.T) {
	dsn := pgcatalog.PrivateDatabase(t, adminDSN(t), "atlantis_console_upgrade")
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Build it the way the console used to, and seed the rows the migrations
	// have to make a decision about.
	if _, err := pool.Exec(ctx, theOldWay); err != nil {
		t.Fatalf("build the old schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS console.audit_log_p209901
  PARTITION OF console.audit_log
  FOR VALUES FROM ('2099-01-01 00:00:00+00') TO ('2099-02-01 00:00:00+00')`); err != nil {
		t.Fatalf("seed partition: %v", err)
	}

	var userID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO console.users (email, password_hash) VALUES ('op@example.com', 'x')
RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// Two audit rows: one by a user who still exists, one by a user already
	// deleted. 0003 takes a different branch for each, and the second has no
	// email left to copy.
	if _, err := pool.Exec(ctx, `
INSERT INTO console.audit_log (user_id, action, created_at)
VALUES ($1, 'approve_plan', '2099-01-15 12:00:00+00'),
       (999999, 'sign_out_all', '2099-01-16 12:00:00+00')`, userID); err != nil {
		t.Fatalf("seed audit rows: %v", err)
	}

	if err := migrate.RunFS(dsn, migrations.Console, "console",
		migrate.ConsoleHistoryTable, quiet()); err != nil {
		t.Fatalf("migrate an existing console database: %v", err)
	}

	// The audit trail survives the identity swap. Migration 0003 drops
	// console.users, and every historical row referenced it by id, so unless the
	// email is copied across first the log keeps every row and can name none of
	// them.
	var actor, actorEmail string
	if err := pool.QueryRow(ctx, `
SELECT actor, actor_email FROM console.audit_log WHERE action = 'approve_plan'`).
		Scan(&actor, &actorEmail); err != nil {
		t.Fatalf("read the migrated audit row: %v", err)
	}
	if want := "local:" + strconv.FormatInt(userID, 10); actor != want {
		t.Errorf("actor = %q, want %q — a pre-migration row must stay "+
			"attributable, and visibly not a Cloud subject", actor, want)
	}
	if actorEmail != "op@example.com" {
		t.Errorf("actor_email = %q, want it copied from console.users before "+
			"that table was dropped", actorEmail)
	}

	// The row whose user was already gone keeps what there was of it.
	var orphanActor, orphanEmail string
	if err := pool.QueryRow(ctx, `
SELECT actor, actor_email FROM console.audit_log WHERE action = 'sign_out_all'`).
		Scan(&orphanActor, &orphanEmail); err != nil {
		t.Fatalf("read the orphaned audit row: %v", err)
	}
	if orphanActor != "local:999999" {
		t.Errorf("orphaned actor = %q, want local:999999", orphanActor)
	}
	if orphanEmail != "" {
		t.Errorf("orphaned actor_email = %q, want empty — there was no user to "+
			"read one from", orphanEmail)
	}

	// console.users is dropped, not left behind holding password hashes nothing
	// reads.
	var usersExists bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'console' AND tablename = 'users')`).
		Scan(&usersExists); err != nil {
		t.Fatalf("look for console.users: %v", err)
	}
	if usersExists {
		t.Error("console.users survived the migration, still holding password hashes")
	}

	// The version is recorded. A no-op that converges the schema and leaves the
	// history empty reads as success until the migration after it.
	var version int
	if err := pool.QueryRow(ctx,
		`SELECT version FROM public.`+migrate.ConsoleHistoryTable).Scan(&version); err != nil {
		t.Fatalf("read migration version: %v", err)
	}
	if version < 3 {
		t.Errorf("history records version %d, want at least 3 — an existing "+
			"database converged but did not record how far it got", version)
	}
}

// An upgraded database and a fresh one end up with the same schema.
//
// Whatever the migrations do, a console running since before they existed ends
// up indistinguishable from one installed today. Otherwise the next migration
// works on one and fails on the other.
//
// Comparing against theOldWay verbatim holds only while the baseline is the
// whole history; migration 0003 changes the shape, which is what it is for.
//
// Compared through the catalogue rather than by reading the SQL: the question
// is what Postgres ended up with, and a migration producing a different default
// or a missing index passes any review of the file.
func TestAnUpgradedDatabaseMatchesAFreshOne(t *testing.T) {
	admin := adminDSN(t)

	upgradedDSN := pgcatalog.PrivateDatabase(t, admin, "atlantis_console_oldway")
	freshDSN := pgcatalog.PrivateDatabase(t, admin, "atlantis_console_newway")
	ctx := context.Background()

	upgradedPool, err := pgxpool.New(ctx, upgradedDSN)
	if err != nil {
		t.Fatalf("connect upgraded: %v", err)
	}
	t.Cleanup(upgradedPool.Close)

	// The old schema, then every migration on top of it.
	if _, err := upgradedPool.Exec(ctx, theOldWay); err != nil {
		t.Fatalf("build the old schema: %v", err)
	}
	if err := migrate.RunFS(upgradedDSN, migrations.Console, "console",
		migrate.ConsoleHistoryTable, quiet()); err != nil {
		t.Fatalf("migrate the upgraded database: %v", err)
	}

	// The same migrations against an empty database.
	if err := migrate.RunFS(freshDSN, migrations.Console, "console",
		migrate.ConsoleHistoryTable, quiet()); err != nil {
		t.Fatalf("migrate a fresh database: %v", err)
	}
	freshPool, err := pgxpool.New(ctx, freshDSN)
	if err != nil {
		t.Fatalf("connect fresh: %v", err)
	}
	t.Cleanup(freshPool.Close)

	upgraded := consoleShape(t, upgradedPool)
	fresh := consoleShape(t, freshPool)
	if upgraded != fresh {
		t.Errorf("an upgraded console and a fresh one have different schemas.\n"+
			"The next migration would work on one and fail on the other.\n\n"+
			"upgraded:\n%s\n\nfresh:\n%s", upgraded, fresh)
	}
}

// pgIdent returns the connected role's name, quoted for use as an identifier.
// Used to create a schema named after the role, which is what makes the
// search_path hazard reachable.
func pgIdent(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var role string
	if err := pool.QueryRow(context.Background(),
		`SELECT quote_ident(current_user)`).Scan(&role); err != nil {
		t.Fatalf("read current_user: %v", err)
	}
	return role
}

// consoleShape renders every column and index in the console schema as a
// stable, comparable string.
func consoleShape(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	var lines []string

	rows, err := pool.Query(ctx, `
SELECT table_name, column_name, data_type, is_nullable, coalesce(column_default, '')
  FROM information_schema.columns
 WHERE table_schema = 'console'
 ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	for rows.Next() {
		var tbl, col, typ, null, def string
		if err := rows.Scan(&tbl, &col, &typ, &null, &def); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		lines = append(lines, "col "+tbl+"."+col+" "+typ+" null="+null+" default="+def)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("columns: %v", err)
	}

	idx, err := pool.Query(ctx, `
SELECT indexdef FROM pg_indexes WHERE schemaname = 'console' ORDER BY indexname`)
	if err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	for idx.Next() {
		var def string
		if err := idx.Scan(&def); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		lines = append(lines, "idx "+def)
	}
	idx.Close()
	if err := idx.Err(); err != nil {
		t.Fatalf("indexes: %v", err)
	}

	// Partitioning strategy, which no column or index query reveals and which
	// the retention worker depends on.
	part, err := pool.Query(ctx, `
SELECT c.relname, pg_get_partkeydef(c.oid)
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'console' AND c.relkind = 'p'
 ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("read partitioning: %v", err)
	}
	for part.Next() {
		var name, key string
		if err := part.Scan(&name, &key); err != nil {
			t.Fatalf("scan partitioning: %v", err)
		}
		lines = append(lines, "part "+name+" "+key)
	}
	part.Close()
	if err := part.Err(); err != nil {
		t.Fatalf("partitioning: %v", err)
	}

	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// Running twice changes nothing.
//
// Without the search_path pin, golang-migrate creates its version table
// wherever search_path resolves, finds it empty on the next connection, and
// replays the whole history — survivable only while every statement is
// CREATE ... IF NOT EXISTS, and not past the first ALTER.
func TestASecondMigrateIsANoOp(t *testing.T) {
	dsn := pgcatalog.PrivateDatabase(t, adminDSN(t), "atlantis_console_twice")
	ctx := context.Background()

	// The condition the pin exists for, which a bare private database does not
	// have.
	//
	// The default search_path is `"$user", public`, so the hazard needs a schema
	// named after the connecting role: the unqualified history table resolves
	// there, finds nothing, and golang-migrate reads the database as never
	// migrated. Production has both — the role is `atlantis` and the infra tree
	// creates a schema called `atlantis` — and a fresh test database has
	// neither, so the test passes with or without the pin until this runs.
	setup, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for setup: %v", err)
	}
	if _, err := setup.Exec(ctx,
		`CREATE SCHEMA IF NOT EXISTS `+pgIdent(t, setup)); err != nil {
		t.Fatalf("create the role-named schema: %v", err)
	}
	setup.Close()

	for i := range 2 {
		if err := migrate.RunFS(dsn, migrations.Console, "console",
			migrate.ConsoleHistoryTable, quiet()); err != nil {
			t.Fatalf("migrate run %d: %v", i+1, err)
		}
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	var rows, dirty int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE dirty) FROM public.`+
			migrate.ConsoleHistoryTable).Scan(&rows, &dirty); err != nil {
		t.Fatalf("read history: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d rows in the migration history, want 1. More than one means "+
			"the runner lost track of where it was and started again", rows)
	}
	if dirty != 0 {
		t.Errorf("the history is marked dirty after two clean runs")
	}
}

// An empty embedded tree is an error, not a silent success. A binary that
// carries no schema and starts anyway reports a successful migration against an
// empty database, and the first query is what says otherwise.
func TestAnEmptyEmbeddedTreeIsRefused(t *testing.T) {
	err := migrate.RunFS("postgres://unused/db", migrations.Console, "nosuchdir",
		migrate.ConsoleHistoryTable, quiet())
	if err == nil {
		t.Fatal("RunFS accepted a directory that is not in the embedded tree, " +
			"so a binary carrying no migrations would start as though migrated")
	}
	if !strings.Contains(err.Error(), "nosuchdir") {
		t.Errorf("the error does not name the missing tree: %v", err)
	}
}
