// Package rehearse builds and destroys the disposable database clones a
// migration is rehearsed against: the managed database's schema and rows,
// copied under one snapshot, so executing the real up_sql there answers what
// the real apply would do.
//
// Everything is pure Go over the wire protocol. The server image carries no
// pg_dump, and client tools would pin this binary to one Postgres major;
// COPY TO/FROM and catalog reads work against every major the server does.
//
// A clone is never read by policy: verdicts are recorded in
// atlantis.rehearsals by the caller, and the clone is dropped on every exit
// path with a registry-backed reaper behind it. A leaked clone costs disk.
package rehearse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Options bounds one clone. The zero value refuses nothing; callers pass
// their configured limits.
type Options struct {
	// CloneTimeout caps the whole clone (schema + snapshot copy). The
	// snapshot transaction holds back vacuum on the source for its
	// duration, so it must be bounded.
	CloneTimeout time.Duration

	// MaxBytes refuses the clone before it starts when the source's tables
	// exceed it.
	MaxBytes int64
}

// ErrTooLarge reports the source exceeding Options.MaxBytes. The caller
// records unverified(too_large) — a refusal, never a partial verdict.
var ErrTooLarge = errors.New("rehearse: the managed database exceeds the clone size ceiling")

// ErrTargetMismatch reports the rehearsal target running a different
// Postgres major than the source. A clone there produces structural
// verdicts the real apply would not.
var ErrTargetMismatch = errors.New("rehearse: the rehearsal target's server version differs from the managed database's")

// systemSchemas are excluded from every catalog walk.
const systemSchemas = "('pg_catalog', 'information_schema', 'pg_toast')"

// notExtensionOwned excludes objects an extension installed — Timescale's
// catalog, pgvector's internals. CREATE EXTENSION on the clone already
// created and seeded them, so copying their rows collides with the seed.
// The placeholder is the column carrying the object's oid.
const notExtensionOwned = `NOT EXISTS (
	SELECT 1 FROM pg_depend dep JOIN pg_extension ext ON dep.refobjid = ext.oid
	WHERE dep.classid = 'pg_class'::regclass AND dep.objid = %s AND dep.deptype = 'e')`

// CheckTarget compares the target's server version against the source's.
func CheckTarget(ctx context.Context, source *pgxpool.Pool, target *pgx.ConnConfig) error {
	var srcVer string
	if err := source.QueryRow(ctx, `SELECT current_setting('server_version_num')`).Scan(&srcVer); err != nil {
		return fmt.Errorf("rehearse: read source version: %w", err)
	}
	conn, err := pgx.ConnectConfig(ctx, target)
	if err != nil {
		return fmt.Errorf("rehearse: connect target: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var tgtVer string
	if err := conn.QueryRow(ctx, `SELECT current_setting('server_version_num')`).Scan(&tgtVer); err != nil {
		return fmt.Errorf("rehearse: read target version: %w", err)
	}
	// Majors must match; the last two digits are the minor.
	if len(srcVer) < 3 || len(tgtVer) < 3 || srcVer[:len(srcVer)-2] != tgtVer[:len(tgtVer)-2] {
		return fmt.Errorf("%w (source %s, target %s)", ErrTargetMismatch, srcVer, tgtVer)
	}
	return nil
}

// Clone creates dbName on the target and fills it: setup builds the schema
// (extensions, then the DDL emitted from the current IR), then every table's
// rows are copied from source under one REPEATABLE READ snapshot, then
// sequence positions.
//
// Both sides run with row_security off. The tables' owner may set that even
// past FORCE ROW LEVEL SECURITY, and without it every `partition by` table
// reads and loads as empty — no tenant is bound, so the policy matches
// nothing.
//
// Foreign keys are captured and dropped on the clone before the load and
// re-added after: rows arrive in catalog order, not dependency order, and
// cycles have no dependency order at all. The re-add re-validates the copied
// data.
func Clone(ctx context.Context, source *pgxpool.Pool, target *pgx.ConnConfig, dbName string, setup func(context.Context, *pgx.Conn) error, opt Options) error {
	if opt.CloneTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opt.CloneTimeout)
		defer cancel()
	}

	if opt.MaxBytes > 0 {
		var bytes int64
		err := source.QueryRow(ctx, `
SELECT COALESCE(sum(pg_total_relation_size(c.oid)), 0)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'r' AND n.nspname NOT IN `+systemSchemas).Scan(&bytes)
		if err != nil {
			return fmt.Errorf("rehearse: size pre-check: %w", err)
		}
		if bytes > opt.MaxBytes {
			return fmt.Errorf("%w: %d bytes against a ceiling of %d", ErrTooLarge, bytes, opt.MaxBytes)
		}
	}

	admin, err := pgx.ConnectConfig(ctx, target)
	if err != nil {
		return fmt.Errorf("rehearse: connect target: %w", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize()); err != nil {
		_ = admin.Close(context.Background())
		return fmt.Errorf("rehearse: create clone database: %w", err)
	}
	_ = admin.Close(context.Background())

	clone, err := connectTo(ctx, target, dbName)
	if err != nil {
		return err
	}
	defer func() { _ = clone.Close(context.Background()) }()
	if _, err := clone.Exec(ctx, `SET row_security = off`); err != nil {
		return fmt.Errorf("rehearse: clone session: %w", err)
	}
	if err := setup(ctx, clone); err != nil {
		return fmt.Errorf("rehearse: apply schema to clone: %w", err)
	}

	// Foreign keys off the clone catalog, defs kept for the re-add.
	type fk struct{ table, name, def string }
	var fks []fk
	rows, err := clone.Query(ctx, `
SELECT conrelid::regclass::text, conname, pg_get_constraintdef(oid)
FROM pg_constraint
WHERE contype = 'f'
  AND connamespace::regnamespace::text NOT IN `+systemSchemas+`
  AND `+fmt.Sprintf(notExtensionOwned, "conrelid"))
	if err != nil {
		return fmt.Errorf("rehearse: list clone foreign keys: %w", err)
	}
	for rows.Next() {
		var f fk
		if err := rows.Scan(&f.table, &f.name, &f.def); err != nil {
			rows.Close()
			return err
		}
		fks = append(fks, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, f := range fks {
		if _, err := clone.Exec(ctx, fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s",
			f.table, pgx.Identifier{f.name}.Sanitize())); err != nil {
			return fmt.Errorf("rehearse: drop %s on %s: %w", f.name, f.table, err)
		}
	}

	// One snapshot across every table.
	srcConn, err := source.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("rehearse: acquire source connection: %w", err)
	}
	defer srcConn.Release()
	tx, err := srcConn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("rehearse: open snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET LOCAL row_security = off`); err != nil {
		return fmt.Errorf("rehearse: source session: %w", err)
	}

	tables, err := cloneTables(ctx, clone)
	if err != nil {
		return err
	}
	for _, t := range tables {
		if err := copyTable(ctx, tx, clone, t); err != nil {
			return err
		}
	}
	if err := copySequences(ctx, tx, clone); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rehearse: close snapshot: %w", err)
	}

	// The re-add validates every copied row against every copied parent.
	for _, f := range fks {
		if _, err := clone.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s",
			f.table, pgx.Identifier{f.name}.Sanitize(), f.def)); err != nil {
			return fmt.Errorf("rehearse: restore %s on %s: %w", f.name, f.table, err)
		}
	}
	return nil
}

type table struct {
	schema, name string
	columns      []string
}

func cloneTables(ctx context.Context, clone *pgx.Conn) ([]table, error) {
	rows, err := clone.Query(ctx, `
SELECT n.nspname, c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'r' AND n.nspname NOT IN `+systemSchemas+`
  AND `+fmt.Sprintf(notExtensionOwned, "c.oid")+`
ORDER BY n.nspname, c.relname`)
	if err != nil {
		return nil, fmt.Errorf("rehearse: list clone tables: %w", err)
	}
	var out []table
	for rows.Next() {
		var t table
		if err := rows.Scan(&t.schema, &t.name); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		cols, err := columnsOf(ctx, clone, out[i].schema, out[i].name)
		if err != nil {
			return nil, err
		}
		out[i].columns = cols
	}
	return out, nil
}

func columnsOf(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, schema, name string) ([]string, error) {
	rows, err := q.Query(ctx, `
SELECT column_name FROM information_schema.columns
WHERE table_schema = $1 AND table_name = $2 AND is_generated = 'NEVER'
ORDER BY ordinal_position`, schema, name)
	if err != nil {
		return nil, fmt.Errorf("rehearse: columns of %s.%s: %w", schema, name, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// copyTable streams one table, binary COPY to binary COPY, restricted to the
// columns both sides hold: the source may carry parked columns the IR — and
// therefore the clone — does not, and their rows are not what the rehearsed
// SQL touches.
func copyTable(ctx context.Context, src pgx.Tx, clone *pgx.Conn, t table) error {
	rel := pgx.Identifier{t.schema, t.name}.Sanitize()

	// A table holding rows before the load was seeded by the clone's own
	// bootstrap — a migration's grants, the change policy defaults — and
	// copying the source's rows over the seed collides with it. Entity
	// tables are always empty here; they are what the copy is for.
	var seeded bool
	if err := clone.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+rel+")").Scan(&seeded); err != nil {
		return fmt.Errorf("rehearse: probe %s: %w", rel, err)
	}
	if seeded {
		return nil
	}

	srcCols, err := columnsOf(ctx, src, t.schema, t.name)
	if err != nil {
		return err
	}
	if len(srcCols) == 0 {
		// The table is new in this IR, or drifted away on the source. An
		// empty table is what the real apply would build on either way.
		return nil
	}
	have := make(map[string]bool, len(srcCols))
	for _, c := range srcCols {
		have[c] = true
	}
	var cols []string
	for _, c := range t.columns {
		if have[c] {
			cols = append(cols, pgx.Identifier{c}.Sanitize())
		}
	}
	if len(cols) == 0 {
		return nil
	}
	colList := strings.Join(cols, ", ")

	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := src.Conn().PgConn().CopyTo(ctx, pw,
			fmt.Sprintf("COPY %s (%s) TO STDOUT (FORMAT binary)", rel, colList))
		_ = pw.CloseWithError(err)
		done <- err
	}()
	_, inErr := clone.PgConn().CopyFrom(ctx, pr,
		fmt.Sprintf("COPY %s (%s) FROM STDIN (FORMAT binary)", rel, colList))
	_ = pr.CloseWithError(inErr)
	outErr := <-done
	if inErr != nil {
		return fmt.Errorf("rehearse: load %s: %w", rel, inErr)
	}
	if outErr != nil {
		return fmt.Errorf("rehearse: read %s: %w", rel, outErr)
	}
	return nil
}

// copySequences moves every sequence position: COPY does not advance them,
// and a rehearsed statement that inserts would otherwise collide with the
// copied rows.
func copySequences(ctx context.Context, src pgx.Tx, clone *pgx.Conn) error {
	rows, err := clone.Query(ctx, `
SELECT n.nspname, c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'S' AND n.nspname NOT IN `+systemSchemas+`
  AND `+fmt.Sprintf(notExtensionOwned, "c.oid"))
	if err != nil {
		return fmt.Errorf("rehearse: list sequences: %w", err)
	}
	type seq struct{ schema, name string }
	var seqs []seq
	for rows.Next() {
		var s seq
		if err := rows.Scan(&s.schema, &s.name); err != nil {
			rows.Close()
			return err
		}
		seqs = append(seqs, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, s := range seqs {
		rel := pgx.Identifier{s.schema, s.name}.Sanitize()
		var last int64
		var called bool
		err := src.QueryRow(ctx, "SELECT last_value, is_called FROM "+rel).Scan(&last, &called)
		if err != nil {
			// Absent on the source: the sequence belongs to a table the IR
			// added and the source has not applied. Its zero position is
			// right.
			continue
		}
		if _, err := clone.Exec(ctx,
			"SELECT setval($1, $2, $3)", strings.TrimSpace(rel), last, called); err != nil {
			return fmt.Errorf("rehearse: setval %s: %w", rel, err)
		}
	}
	return nil
}

// Outcome is what executing the rehearsed SQL on the clone produced.
type Outcome struct {
	// Err is nil when the SQL completed. Otherwise PgErr carries the
	// structured failure when Postgres reported one.
	Err   error
	PgErr *pgconn.PgError

	// TimedOut reports the statement_timeout firing (SQLSTATE 57014).
	TimedOut bool
}

// Execute runs upSQL inside a transaction on the clone and rolls it back, so
// diagnostics afterwards see the pre-migration rows — the data a remediation
// has to fix.
func Execute(ctx context.Context, target *pgx.ConnConfig, dbName, upSQL string, timeout time.Duration) (Outcome, error) {
	conn, err := connectTo(ctx, target, dbName)
	if err != nil {
		return Outcome{}, err
	}
	defer func() { _ = conn.Close(context.Background()) }()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return Outcome{}, fmt.Errorf("rehearse: begin execute: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if timeout > 0 {
		if _, err := tx.Exec(ctx,
			fmt.Sprintf("SET LOCAL statement_timeout = %d", timeout.Milliseconds())); err != nil {
			return Outcome{}, fmt.Errorf("rehearse: set timeout: %w", err)
		}
	}

	_, execErr := tx.Exec(ctx, upSQL)
	out := Outcome{Err: execErr}
	var pgErr *pgconn.PgError
	if errors.As(execErr, &pgErr) {
		out.PgErr = pgErr
		out.TimedOut = pgErr.Code == "57014"
	}
	return out, nil
}

// CountWhere answers one diagnostic count on the clone, for the remediation
// counts a failure makes useful. Identifiers arrive pre-sanitized relation
// and column text from the caller's catalog reads.
func CountWhere(ctx context.Context, target *pgx.ConnConfig, dbName, rel, predicate string) (int64, error) {
	conn, err := connectTo(ctx, target, dbName)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, `SET row_security = off`); err != nil {
		return 0, err
	}
	var n int64
	err = conn.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE %s", rel, predicate)).Scan(&n)
	return n, err
}

// Drop destroys one clone database, terminating anything still attached.
func Drop(ctx context.Context, target *pgx.ConnConfig, dbName string) error {
	admin, err := pgx.ConnectConfig(ctx, target)
	if err != nil {
		return fmt.Errorf("rehearse: connect target: %w", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	_, _ = admin.Exec(ctx, `
SELECT pg_terminate_backend(pid) FROM pg_stat_activity
WHERE datname = $1 AND pid <> pg_backend_pid()`, dbName)
	if _, err := admin.Exec(ctx,
		"DROP DATABASE IF EXISTS "+pgx.Identifier{dbName}.Sanitize()+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("rehearse: drop clone: %w", err)
	}
	return nil
}

// ListClones names every database on the target matching the clone prefix,
// for the reaper's convention sweep — the case a restored control database
// no longer remembers.
func ListClones(ctx context.Context, target *pgx.ConnConfig, prefix string) ([]string, error) {
	admin, err := pgx.ConnectConfig(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("rehearse: connect target: %w", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	rows, err := admin.Query(ctx,
		`SELECT datname FROM pg_database WHERE datname LIKE $1 || '%'`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// connectTo opens a connection to one database on the target.
func connectTo(ctx context.Context, target *pgx.ConnConfig, dbName string) (*pgx.Conn, error) {
	cfg := target.Copy()
	cfg.Database = dbName
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("rehearse: connect clone %s: %w", dbName, err)
	}
	return conn, nil
}
