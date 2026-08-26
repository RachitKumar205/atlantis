// Package embedded is the fidelity backend for the atlantis sandbox.
//
// It runs a real Postgres in-process through fergusstrange/embedded-postgres
// and exposes it as runtime.Pool: a 4–8 second cold start against the sim's
// sub-millisecond one, in exchange for every PG idiom working — LATERAL,
// recursive CTE, tsvector, pg_trgm, jsonb_array_elements, a real HNSW index.
// The sim under internal/runtime/sandbox/sim parses a whitelist instead.
//
// sandbox.New routes here when Options.Backend is BackendEmbedded, and when
// BackendAuto sees a schema carrying custom query, procedure or hypertable
// blocks, which the sim's whitelist cannot honour.
//
// The IR's CREATE TABLE, CREATE INDEX and trigger DDL is applied at boot by
// internal/codegen's emitter, called with an empty old IR so every entity comes
// out ClassAdditive. That makes the schema here byte-equivalent to what a real
// `tide apply` produces.
//
// Not supported, and served by the sim instead:
//
//   - Mark / RestoreTo. Time travel would need pg_dump and pg_restore.
//   - Fork, for the same reason.
//
// The Outbox and Cache stubs do not apply: real handlers point at the embedded
// URL and run against it.
//
// Each backend spawns its own PG process and data directory, and every
// fixtures.Bulk or CRUD call is a round trip.
package embedded

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// ErrUnsupportedOperation is returned by methods that don't make sense
// on the embedded backend (Mark, Fork, etc. — see package doc).
var ErrUnsupportedOperation = errors.New("sandbox embedded: operation not supported (use sim backend)")

// Options tunes the embedded backend. Empty value yields working
// defaults: random free port, 60 s start timeout, discarded logs, and
// a per-instance temp directory for both data and the extracted
// binaries (so parallel instances don't trip over each other's
// cleanup-on-start dance).
type Options struct {
	// Port is the localhost port the embedded PG listens on. 0 →
	// kernel-picked free port. Tests leave this at 0 so parallel
	// instances don't collide.
	Port uint32

	// StartTimeout caps how long Start blocks waiting for PG to
	// come up. 0 → 60 s.
	StartTimeout time.Duration

	// LogOutput receives PG stdout/stderr. nil → io.Discard.
	LogOutput io.Writer

	// DataDir overrides the on-disk PGDATA location. Empty →
	// per-instance temp dir under os.TempDir(). Set this if you
	// want PG state to survive a process restart.
	DataDir string

	// RuntimeDir overrides where embedded-postgres extracts the
	// binary archive. Empty → per-instance temp dir. The library's
	// default is a single shared path that fails cleanup when
	// multiple Backends start back-to-back; we default to unique
	// paths so the failure mode disappears.
	RuntimeDir string
}

// Backend owns one embedded-postgres process + its pgxpool. The
// localPool satisfies runtime.Pool so the sandbox façade can hand it
// back from Sandbox.Pool() without further wrapping.
//
// dataDir and runtimeDir record per-instance temp directories that
// Close removes — without unique directories embedded-postgres's
// cleanup-on-start fails when multiple instances overlap.
type Backend struct {
	mu         sync.Mutex
	pg         *embeddedpostgres.EmbeddedPostgres
	pool       *localPool
	port       uint32
	started    bool
	dataDir    string
	runtimeDir string
}

// New starts an embedded Postgres process and applies the IR's full
// DDL into it. The returned Backend is ready to serve queries; Close
// shuts down the process and removes its data directory.
//
// Cold-start cost: 4–8 s on Linux, 8–12 s on macOS. Tests gate use
// behind build tags or explicit opt-in for that reason.
func New(ctx context.Context, ir *dsl.IR, opts Options) (*Backend, error) {
	if ir == nil {
		return nil, fmt.Errorf("sandbox embedded: IR is required")
	}

	port := opts.Port
	if port == 0 {
		p, err := freePort()
		if err != nil {
			return nil, fmt.Errorf("sandbox embedded: pick port: %w", err)
		}
		port = uint32(p)
	}
	startTimeout := opts.StartTimeout
	if startTimeout == 0 {
		startTimeout = 60 * time.Second
	}
	logOut := opts.LogOutput
	if logOut == nil {
		logOut = io.Discard
	}

	dataDir := opts.DataDir
	if dataDir == "" {
		d, err := makeTempDir("atlantis-sandbox-data-")
		if err != nil {
			return nil, fmt.Errorf("sandbox embedded: tempdir(data): %w", err)
		}
		dataDir = d
	}
	runtimeDir := opts.RuntimeDir
	if runtimeDir == "" {
		d, err := makeTempDir("atlantis-sandbox-runtime-")
		if err != nil {
			return nil, fmt.Errorf("sandbox embedded: tempdir(runtime): %w", err)
		}
		runtimeDir = d
	}

	cfg := embeddedpostgres.DefaultConfig().
		Port(port).
		StartTimeout(startTimeout).
		Logger(logOut).
		DataPath(dataDir).
		RuntimePath(runtimeDir)

	pgInstance := embeddedpostgres.NewDatabase(cfg)
	if err := pgInstance.Start(); err != nil {
		return nil, fmt.Errorf("sandbox embedded: start: %w", err)
	}

	// Two connections, in this order, and the order is the security property.
	//
	// The bootstrap connection is a superuser and exists only long enough to
	// create the schema and the unprivileged role. It is closed before anything
	// outside this function can reach the database. The pool the Backend keeps
	// — the one that runs caller SQL — is not a superuser.
	//
	// This matters because the sandbox executes arbitrary caller SQL by design
	// (Pool().Exec, reachable from cmd/tide and from the console's
	// /api/sandbox/{id}/sql/exec route). A superuser connection there is
	// `COPY ... FROM PROGRAM`, which is remote code execution on whatever host
	// the sandbox runs on, for anyone who can reach that route.
	bootstrapURL := fmt.Sprintf("postgres://postgres:postgres@localhost:%d/postgres?sslmode=disable", port)
	bootstrap, err := newLocalPool(ctx, bootstrapURL)
	if err != nil {
		_ = pgInstance.Stop()
		return nil, fmt.Errorf("sandbox embedded: connect: %w", err)
	}

	b := &Backend{
		pg:         pgInstance,
		pool:       bootstrap,
		port:       port,
		started:    true,
		dataDir:    dataDir,
		runtimeDir: runtimeDir,
	}

	// Schema DDL runs privileged, on a database no caller has touched yet.
	// A CVE-2018-1058 search_path hijack needs the attacker to have planted an
	// object first; here there is no attacker and no prior connection.
	if err := applySchema(ctx, b, ir); err != nil {
		_ = b.Close()
		return nil, err
	}

	password, err := randomPassword()
	if err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("sandbox embedded: %w", err)
	}
	if err := grantSandboxRole(ctx, bootstrap, password, ir); err != nil {
		_ = b.Close()
		return nil, err
	}

	userURL := fmt.Sprintf("postgres://%s:%s@localhost:%d/postgres?sslmode=disable",
		sandboxRole, password, port)
	userPool, err := newLocalPool(ctx, userURL)
	if err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("sandbox embedded: connect as %s: %w", sandboxRole, err)
	}
	b.pool = userPool
	bootstrap.Close()
	return b, nil
}

// sandboxRole is the unprivileged identity every caller-facing sandbox
// connection uses. Named rather than generated so it is greppable and so the
// test asserting `rolsuper = false` has something to assert against.
const sandboxRole = "atlantis_sandbox"

// randomPassword returns a password nothing records. The role is reachable only
// over loopback on a kernel-assigned port, and its lack of privilege is the
// boundary, not this; random so a second process on the host cannot guess it.
func randomPassword() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// grantSandboxRole creates the unprivileged role and gives it exactly the
// access a sandbox needs: read and write the tables the schema just created,
// and nothing else.
//
// Deliberately withheld: SUPERUSER (COPY ... FROM PROGRAM, pg_read_file,
// lo_import), CREATEDB, CREATEROLE, and CREATE on the schema holding the
// generated tables. The role can change rows; it cannot change the shape of the
// world it runs in.
//
// statement_timeout is set on the role rather than per-connection so it applies
// to every session regardless of which code path opened it. Thirty seconds is
// far beyond any legitimate sandbox query and well short of a wedged one.
func grantSandboxRole(ctx context.Context, pool *localPool, password string, ir *dsl.IR) error {
	stmts := []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE`,
			sandboxRole, password),
		fmt.Sprintf(`ALTER ROLE %s SET statement_timeout = '30s'`, sandboxRole),
		fmt.Sprintf(`ALTER ROLE %s SET idle_in_transaction_session_timeout = '60s'`, sandboxRole),
	}
	for _, sch := range irSchemas(ir) {
		stmts = append(stmts,
			fmt.Sprintf(`GRANT USAGE ON SCHEMA %q TO %s`, sch, sandboxRole),
			fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %q TO %s`,
				sch, sandboxRole),
			fmt.Sprintf(`GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA %q TO %s`,
				sch, sandboxRole),
		)
	}
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("sandbox embedded: grant %s: %w", sandboxRole, err)
		}
	}
	return nil
}

// Pool returns the runtime.Pool implementation. Generated handlers can
// be wired straight to this and run against real Postgres.
func (b *Backend) Pool() runtime.Pool { return b.pool }

// Port exposes the listening port so tests / CLIs can construct an
// independent connection (e.g. for `psql` debugging).
func (b *Backend) Port() uint32 { return b.port }

// Close shuts down the embedded process, releases the pool, and
// removes the per-instance temp directories. Idempotent — calling
// Close twice is safe; the second call no-ops.
func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.started {
		return nil
	}
	b.started = false
	if b.pool != nil {
		b.pool.Close()
	}
	if b.pg != nil {
		if err := b.pg.Stop(); err != nil {
			return fmt.Errorf("sandbox embedded: stop: %w", err)
		}
	}
	// Best-effort tempdir cleanup. We don't surface errors because
	// the runtime is already gone; leftover files are at worst a
	// disk-space leak in /tmp, which the OS cleans periodically.
	if b.dataDir != "" {
		_ = os.RemoveAll(b.dataDir)
	}
	if b.runtimeDir != "" {
		_ = os.RemoveAll(b.runtimeDir)
	}
	return nil
}

// applySchema reuses the production DDL emitter to build the schema
// from an empty IR up to the supplied one. The "diff from empty"
// trick gives us every CREATE TABLE / CREATE INDEX / trigger the
// caller's schema implies, without writing a parallel emitter.
//
// Proto numbers aren't assigned in raw IRs; codegen.AssignProtoNumbers
// is called first so EmitSQL sees the same shape codegen-emitted
// migrations land with.
//
// EmitSQL omits CREATE SCHEMA for the canonical "atlantis" schema, which the
// bootstrap migration creates in production. A fresh embedded PG has no
// bootstrap, so CREATE SCHEMA IF NOT EXISTS is prepended for every schema the
// catalog references — idempotent where the emitter already wrote one.
func applySchema(ctx context.Context, b *Backend, ir *dsl.IR) error {
	empty := &dsl.IR{}
	codegen.AssignProtoNumbers(empty, ir)
	d := codegen.ComputeDiff(empty, ir)
	scripts, err := codegen.EmitSQL(empty, ir, d)
	if err != nil {
		return fmt.Errorf("sandbox embedded: emit DDL: %w", err)
	}
	if scripts.Up == "" {
		return nil
	}
	preamble := schemaPreamble(ir)
	full := preprocessDDLForVanillaPG(preamble + scripts.Up)
	if _, err := b.pool.Exec(ctx, full); err != nil {
		return fmt.Errorf("sandbox embedded: apply DDL: %w\n--- SQL ---\n%s", err, full)
	}
	return nil
}

// preprocessDDLForVanillaPG rewrites the codegen-emitted DDL so it
// loads on the stock Postgres binary embedded-postgres ships — which
// has no pgvector, no TimescaleDB. Three transforms:
//
//  1. `vector(N)` columns → `BYTEA`. Column round-trips, but vector
//     similarity operators (<->, <=>, <#>) and pgvector-typed binds
//     fail at query time. Acceptable: schemas using vectors should
//     usually pick the sim backend (which models vectors natively)
//     rather than embedded.
//
//  2. HNSW index DDL is stripped. Without pgvector the access method
//     doesn't exist; the table loads, queries that would have used
//     the index just do sequential scans.
//
//  3. `SELECT create_hypertable(...)` is stripped. The hypertable
//     becomes a regular table — time-based queries still work, just
//     without partition pruning.
//
// All three transforms are best-effort regex passes. The alternative —
// failing the boot — is the worst outcome on real caller schemas that
// universally combine these extensions with otherwise-vanilla DDL.
func preprocessDDLForVanillaPG(ddl string) string {
	ddl = pgvectorTypeRE.ReplaceAllString(ddl, "BYTEA")
	ddl = hnswIndexRE.ReplaceAllString(ddl, "-- HNSW index stripped (no pgvector on embedded)")
	ddl = hypertableRE.ReplaceAllString(ddl, "-- create_hypertable stripped (no TimescaleDB on embedded)")
	return ddl
}

var (
	pgvectorTypeRE = regexp.MustCompile(`(?i)\bvector\s*\(\s*\d+\s*\)`)
	hnswIndexRE    = regexp.MustCompile(`(?is)CREATE\s+INDEX[^;]*?\s+USING\s+hnsw[^;]*?;`)
	hypertableRE   = regexp.MustCompile(`(?is)SELECT\s+create_hypertable\s*\([^;]*?\)\s*;`)
)

// schemaPreamble emits `CREATE SCHEMA IF NOT EXISTS "<name>"` for every
// distinct schema the IR references. EmitSQL elides them for "atlantis", which
// production bootstraps and a fresh embedded PG does not.
func schemaPreamble(ir *dsl.IR) string {
	// Codegen places touch-trigger functions in the "atlantis" schema
	// regardless of which schema the table lives in
	// (`CREATE FUNCTION "atlantis"."ns_entity_touch_fn"() ...`), and
	// each entity's CREATE TRIGGER then references that function. So
	// the atlantis schema must exist even when no entity table is
	// declared inside it — bootstrap it unconditionally.
	var out string
	for _, s := range irSchemas(ir) {
		out += fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %q;\n", s)
	}
	return out
}

// irSchemas returns every distinct schema name the IR places objects in,
// sorted so callers emitting DDL or grants produce stable output.
//
// Shared by schemaPreamble and grantSandboxRole: the set of schemas that get
// created is exactly the set the sandbox role must be able to use, and deriving
// both from one function is what stops a schema being created that the role
// then cannot reach.
func irSchemas(ir *dsl.IR) []string {
	seen := map[string]struct{}{"atlantis": {}}
	for i := range ir.Entities {
		e := &ir.Entities[i]
		// Default schema is "atlantis"; entities with a TableName
		// override might point at a different one.
		schema := "atlantis"
		if e.TableName != "" {
			// table "schema.name" form. dsl.Entity.TableName is the
			// raw modifier; codegen normalizes it. Anything before a
			// dot is the schema.
			if i := indexOf(e.TableName, '.'); i >= 0 {
				schema = e.TableName[:i]
			}
		}
		seen[schema] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// freePort asks the kernel for an unused TCP port by binding, reading the
// assigned port, then closing.
//
// The close-to-listen gap is a TOCTOU window another process can take the port
// in. PG then holds it for its whole life, so the window is one moment per
// backend.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	addr := l.Addr().(*net.TCPAddr)
	_ = l.Close()
	return addr.Port, nil
}

// tempDirPrefixes are the two shapes SweepAbandoned recognises, kept beside
// their creation site. A second copy of these literals elsewhere is how a sweep
// comes to delete a directory it was never told about.
var tempDirPrefixes = []string{"atlantis-sandbox-data-", "atlantis-sandbox-runtime-"}

// ownerFile records which process a sandbox tempdir belongs to. See
// SweepAbandoned for why a directory without one is left alone.
const ownerFile = ".atlantis-owner"

// makeTempDir returns a uniquely-named subdirectory under os.TempDir().
//
// Each Backend gets its own data and runtime pair, so embedded-postgres's
// cleanup-on-start does not fail across instances running back to back.
// Backend.Close removes them.
//
// The owner file is written before the directory is used, so a sweep that
// runs between MkdirAll and the first write still sees a claimed directory.
func makeTempDir(prefix string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	dir := filepath.Join(os.TempDir(), prefix+hex.EncodeToString(b[:]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, ownerFile),
		[]byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return "", fmt.Errorf("claim %s: %w", dir, err)
	}
	return dir, nil
}

// SweepAbandoned removes sandbox tempdirs whose owning process is gone.
//
// A directory is removed only when its owner file names a process that is no
// longer running. Anything else — no owner file, an unreadable one, a PID that
// still resolves — is kept and logged, so the cases this cannot judge leak disk
// rather than deleting a live sandbox.
//
// Globbing the prefixes and removing every match cannot separate a crashed
// predecessor's directory from one in use right now, and this runs on every
// console boot: a rolling restart, a blue/green deploy, or two `make dev` runs
// on one host then delete a live sandbox mid-session, surfacing as an unrelated
// Postgres error. `go test` runs packages concurrently and reaches it the same
// way.
//
// PID reuse falls the same direction: a recycled PID reads as alive and the
// directory is kept.
//
// A directory with no owner file is never swept, and is named in the log.
func SweepAbandoned(logf func(string, ...any)) {
	for _, prefix := range tempDirPrefixes {
		matches, err := filepath.Glob(filepath.Join(os.TempDir(), prefix+"*"))
		if err != nil {
			logf("sandbox sweep: glob %s: %v", prefix, err)
			continue
		}
		for _, dir := range matches {
			raw, err := os.ReadFile(filepath.Join(dir, ownerFile))
			if err != nil {
				logf("sandbox sweep: %s carries no owner and was left in place; "+
					"remove it by hand if it is stale", dir)
				continue
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				logf("sandbox sweep: %s has an unreadable owner (%q) and was left "+
					"in place", dir, string(raw))
				continue
			}
			if processIsRunning(pid) {
				continue
			}
			if err := os.RemoveAll(dir); err != nil {
				logf("sandbox sweep: remove %s: %v", dir, err)
			}
		}
	}
}

// processIsRunning reports whether a PID resolves to a live process.
//
// Signal 0 performs the permission and existence checks without delivering
// anything. EPERM means the process exists under another user, which is still
// "running" for this purpose — and is exactly the multi-operator case the
// sweep must not delete through.
func processIsRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
