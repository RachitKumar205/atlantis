package console

import (
	"context"
	"strings"
	"testing"
)

// verifyConsolePolicies exists because a missing boundary is invisible from
// outside: binding an organisation succeeds whether or not a policy is there,
// and so does every query afterwards. These tests break the boundary in each of
// the ways it can be broken and require the check to say so.
//
// They run against the fixture's real, migrated database and manipulate the
// catalogue directly through the superuser pool, so what is asserted is what
// Postgres ended up with rather than what the migration files say.

func TestPolicyGuardAcceptsACorrectlyIsolatedSchema(t *testing.T) {
	f := newConsoleFixture(t)

	// The control. Without it every case below passes against a check that
	// refuses unconditionally, which would be a console that never starts.
	if err := verifyConsolePolicies(context.Background(), f.srv.db.pool); err != nil {
		t.Fatalf("the migrated schema was rejected: %v", err)
	}
}

func TestPolicyGuardCatchesABrokenBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ string // SQL that breaks the boundary
		repair string
		want   string // substring the refusal must contain
	}{
		{
			name:   "the policy is gone",
			break_: `DROP POLICY audit_log_org_isolation ON console.audit_log`,
			repair: `CREATE POLICY audit_log_org_isolation ON console.audit_log AS RESTRICTIVE
			         USING (org = console.current_org()) WITH CHECK (org = console.current_org())`,
			want: "no RESTRICTIVE policy",
		},
		{
			// The trap worth naming: ENABLE without FORCE leaves the owning
			// role exempt, and the console's role owns these tables. \d lists
			// the policy and it applies to nobody who connects.
			name:   "enabled but not forced",
			break_: `ALTER TABLE console.audit_log NO FORCE ROW LEVEL SECURITY`,
			repair: `ALTER TABLE console.audit_log FORCE ROW LEVEL SECURITY`,
			want:   "not FORCED",
		},
		{
			name:   "row-level security switched off entirely",
			break_: `ALTER TABLE console.audit_log DISABLE ROW LEVEL SECURITY`,
			repair: `ALTER TABLE console.audit_log ENABLE ROW LEVEL SECURITY`,
			want:   "no row-level security",
		},
		{
			// The boundary must be RESTRICTIVE. A permissive one ORs with any
			// other permissive policy, so the default grant alone would widen
			// straight past it — the table would look policed and isolate
			// nothing.
			name: "the boundary is permissive rather than restrictive",
			break_: `DROP POLICY audit_log_org_isolation ON console.audit_log;
			         CREATE POLICY audit_log_org_isolation ON console.audit_log AS PERMISSIVE
			           USING (org = console.current_org()) WITH CHECK (org = console.current_org())`,
			repair: `DROP POLICY audit_log_org_isolation ON console.audit_log;
			         CREATE POLICY audit_log_org_isolation ON console.audit_log AS RESTRICTIVE
			           USING (org = console.current_org()) WITH CHECK (org = console.current_org())`,
			want: "no RESTRICTIVE policy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newConsoleFixture(t)
			ctx := context.Background()

			if _, err := f.pool.Exec(ctx, tc.break_); err != nil {
				t.Fatalf("break the boundary: %v", err)
			}
			t.Cleanup(func() {
				if _, err := f.pool.Exec(context.Background(), tc.repair); err != nil {
					t.Logf("repair failed (the database is torn down anyway): %v", err)
				}
			})

			err := verifyConsolePolicies(ctx, f.srv.db.pool)
			if err == nil {
				t.Fatal("the check passed on a schema with no working boundary")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal does not say what is wrong.\ngot:  %v\nwant it to contain: %q", err, tc.want)
			}
			// Attributable rather than merely non-nil: a test satisfied by any
			// error passes with the check replaced by `return errors.New("no")`.
			if !strings.Contains(err.Error(), "audit_log") {
				t.Errorf("refusal does not name the table: %v", err)
			}
		})
	}
}

// TestPolicyGuardCatchesANewUnpolicedTable is the case that actually happens.
//
// Nobody removes a policy. Somebody adds a table, does not think about
// isolation, and it holds several organisations' rows from its first row on.
// Requiring every table to be either policed or listed in unpolicedTables turns
// that from an oversight into a boot failure.
func TestPolicyGuardCatchesANewUnpolicedTable(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx,
		`CREATE TABLE console.notifications (id bigserial PRIMARY KEY, body text NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	err := verifyConsolePolicies(ctx, f.srv.db.pool)
	if err == nil {
		t.Fatal("a new table with no organisation boundary was accepted")
	}
	if !strings.Contains(err.Error(), "notifications") {
		t.Errorf("refusal does not name the new table: %v", err)
	}
	// The message has to say what to do, or the next person deletes the check.
	if !strings.Contains(err.Error(), "unpolicedTables") {
		t.Errorf("refusal does not say how to record a deliberate exemption: %v", err)
	}
}

// TestPolicyGuardIgnoresPartitions. audit_log's children are created at runtime
// and carry no policy of their own by design — they are forced with no policy,
// which is deny-all on direct access. Flagging them would make the console
// refuse to start every month.
func TestPolicyGuardIgnoresPartitions(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	var partitions int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'console' AND c.relispartition`).Scan(&partitions); err != nil {
		t.Fatalf("count partitions: %v", err)
	}
	if partitions == 0 {
		t.Fatal("no partitions exist, so this test would pass whatever the check did")
	}

	if err := verifyConsolePolicies(ctx, f.srv.db.pool); err != nil {
		t.Fatalf("the check flagged %d partition(s): %v", partitions, err)
	}
}
