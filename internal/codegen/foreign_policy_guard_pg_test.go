package codegen

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/storage/pg"
)

// The migration must refuse rather than claim isolation it cannot deliver.
//
// PostgreSQL ORs permissive policies. A table carrying a legacy blanket policy
// — the shape a database being adopted FOR tenant isolation plausibly already
// has — applied this migration cleanly, reported success, and isolated nothing.
// A review executed it: the acme-bound caller read every tenant's rows and its
// forged cross-tenant INSERT was accepted, while the plan text said "a request
// that carries no tenant reads nothing from this table".
func TestMigrationRefusesWhenAnotherPermissivePolicyExists(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the foreign-policy guard")
	}
	ctx := context.Background()
	admin, err := pg.New(ctx, pg.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)

	drop := func() { _, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.pguard_doc CASCADE`) }
	drop()
	t.Cleanup(drop)

	before := lower(t, `
entity Doc in pguard {
  id     bigint primary
  tenant varchar(16) not null
  body   text
}
`)
	after := lower(t, `
entity Doc in pguard {
  id     bigint primary
  tenant varchar(16) not null
  body   text
  partition by tenant
}
`)
	AssignProtoNumbers(nil, before)
	AssignProtoNumbers(before, after)

	initial, err := EmitInitial(before)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := admin.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("apply initial: %v", err)
	}
	scripts, err := EmitSQL(before, after, ComputeDiff(before, after))
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}

	t.Run("a clean table applies", func(t *testing.T) {
		if _, err := admin.Exec(ctx, scripts.Up); err != nil {
			t.Fatalf("the guard blocked a table with no other policy: %v", err)
		}
		if _, err := admin.Exec(ctx, scripts.Down); err != nil {
			t.Fatalf("down: %v", err)
		}
	})

	t.Run("a legacy blanket policy stops the migration", func(t *testing.T) {
		if _, err := admin.Exec(ctx,
			`ALTER TABLE atlantis.pguard_doc ENABLE ROW LEVEL SECURITY;
			 CREATE POLICY legacy_app_access ON atlantis.pguard_doc FOR ALL USING (true) WITH CHECK (true);`); err != nil {
			t.Fatalf("seed the legacy policy: %v", err)
		}
		_, err := admin.Exec(ctx, scripts.Up)
		if err == nil {
			t.Fatal("the migration applied and reported success while isolating " +
				"nothing. Permissive policies are OR'd, so the legacy USING (true) " +
				"policy means every caller still reads every tenant's rows")
		}
		// The refusal has to name the policy, or the operator cannot act.
		if !strings.Contains(err.Error(), "legacy_app_access") {
			t.Errorf("the refusal does not name the offending policy: %v", err)
		}
		if !strings.Contains(err.Error(), "RESTRICTIVE") {
			t.Errorf("the refusal does not say what to do about it: %v", err)
		}
	})

	t.Run("a RESTRICTIVE policy is fine", func(t *testing.T) {
		if _, err := admin.Exec(ctx, `
DROP POLICY legacy_app_access ON atlantis.pguard_doc;
CREATE POLICY narrowing ON atlantis.pguard_doc AS RESTRICTIVE USING (body IS NOT NULL);`); err != nil {
			t.Fatalf("swap to restrictive: %v", err)
		}
		if _, err := admin.Exec(ctx, scripts.Up); err != nil {
			t.Fatalf("a RESTRICTIVE policy can only narrow, so it must not block "+
				"the migration: %v", err)
		}
	})
}
