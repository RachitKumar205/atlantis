package embedded

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// The sandbox runs arbitrary caller SQL by design — Pool().Exec is reachable
// from `tide sandbox` and from the console's /api/sandbox/{id}/sql/exec route.
// It therefore must never hold a superuser connection, because superuser plus
// arbitrary SQL is `COPY ... FROM PROGRAM`, which is remote code execution on
// whatever host the sandbox runs on, for anyone who can reach that route.
//
// That was the state of this code until 2026-07-30. These tests exist so it
// cannot return quietly.

func testIR() *dsl.IR {
	return &dsl.IR{Entities: []dsl.Entity{{
		Name:      "Widget",
		Namespace: "sbx",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "label", Type: dsl.FieldType{Name: "text"}, NotNull: true},
		},
	}}}
}

// TestSandboxPoolIsNotSuperuser is the single load-bearing assertion. Almost
// every other privilege check below passes only because this one holds, so it
// is pinned directly rather than inferred from its consequences.
func TestSandboxPoolIsNotSuperuser(t *testing.T) {
	if testing.Short() {
		t.Skip("boots an embedded Postgres")
	}
	ctx := context.Background()
	b, err := New(ctx, testIR(), Options{})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer func() { _ = b.Close() }()

	var super bool
	row := b.Pool().QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`)
	if err := row.Scan(&super); err != nil {
		t.Fatalf("read rolsuper: %v", err)
	}
	if super {
		t.Fatal("the sandbox pool is a superuser; arbitrary caller SQL is host RCE")
	}

	var user string
	if err := b.Pool().QueryRow(ctx, `SELECT current_user`).Scan(&user); err != nil {
		t.Fatalf("read current_user: %v", err)
	}
	if user != sandboxRole {
		t.Errorf("connected as %q, want %q", user, sandboxRole)
	}
}

// TestSandboxCannotExecuteProgramsOrReadFiles drives the actual escalations
// rather than trusting that a non-superuser implies them.
//
// Asserted on SQLSTATE 42501 (insufficient_privilege) rather than on message
// text. Text varies between Postgres versions — this embedded build says
// "permission denied to COPY to or from an external program" where PG17 names
// the pg_execute_server_program role — and a test that pins wording fails on an
// upgrade for no reason. The code still discriminates: a malformed statement
// returns 42601 (syntax_error), so "it errored" cannot pass by accident.
func TestSandboxCannotExecuteProgramsOrReadFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("boots an embedded Postgres")
	}
	ctx := context.Background()
	b, err := New(ctx, testIR(), Options{})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer func() { _ = b.Close() }()

	if _, err := b.Pool().Exec(ctx, `CREATE TEMP TABLE rce(t text)`); err != nil {
		t.Fatalf("temp table: %v", err)
	}

	for _, tc := range []struct{ name, sql string }{
		{"COPY FROM PROGRAM is host RCE", `COPY rce FROM PROGRAM 'echo pwned'`},
		{"COPY TO PROGRAM is host RCE", `COPY rce TO PROGRAM 'cat > /tmp/pwned'`},
		{"COPY FROM file reads the host", `COPY rce FROM '/etc/passwd'`},
		{"pg_read_file reads the host", `SELECT pg_read_file('/etc/passwd')`},
		{"lo_import reads the host", `SELECT lo_import('/etc/passwd')`},
		{"extensions could re-open the above", `CREATE EXTENSION dblink`},
		{"self-promotion to superuser", `ALTER ROLE ` + sandboxRole + ` SUPERUSER`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := b.Pool().Exec(ctx, tc.sql)
			if err == nil {
				t.Fatalf("%s succeeded — the sandbox can escalate", tc.sql)
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("not a Postgres error, so not a privilege denial: %v", err)
			}
			if pgErr.Code != insufficientPrivilege {
				t.Errorf("denied with SQLSTATE %s, want %s (insufficient_privilege): %v",
					pgErr.Code, insufficientPrivilege, err)
			}
		})
	}
}

// insufficientPrivilege is SQLSTATE 42501. Distinct from 42601 (syntax_error),
// which is what a broken test statement would produce.
const insufficientPrivilege = "42501"

// The lockdown is worthless if it also breaks the sandbox. A caller must still
// be able to do everything a sandbox is for.
func TestSandboxCanStillDoItsJob(t *testing.T) {
	if testing.Short() {
		t.Skip("boots an embedded Postgres")
	}
	ctx := context.Background()
	b, err := New(ctx, testIR(), Options{})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer func() { _ = b.Close() }()

	if _, err := b.Pool().Exec(ctx,
		`INSERT INTO atlantis.sbx_widget (id, label) VALUES (1, 'ok')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var label string
	if err := b.Pool().QueryRow(ctx,
		`SELECT label FROM atlantis.sbx_widget WHERE id = 1`).Scan(&label); err != nil {
		t.Fatalf("select: %v", err)
	}
	if label != "ok" {
		t.Errorf("read back %q, want %q", label, "ok")
	}

	// Constraints are real — this is the whole reason to use embedded rather
	// than the sim, and it must survive the privilege drop.
	_, err = b.Pool().Exec(ctx, `INSERT INTO atlantis.sbx_widget (id, label) VALUES (2, NULL)`)
	if err == nil {
		t.Fatal("NOT NULL was not enforced")
	}
	if !strings.Contains(err.Error(), "not-null") && !strings.Contains(err.Error(), "not null") {
		t.Errorf("expected a NOT NULL violation, got %v", err)
	}
}
