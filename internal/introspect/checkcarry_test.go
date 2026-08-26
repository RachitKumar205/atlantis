package introspect

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Adopting a database whose CHECK constraints already exist must not report
// them as drift to be added.
//
// FromPostgres supplies column facts and carries everything else across from
// the declaration, entity-level and field-level Checks included. Carrying only
// one of the two makes diffChecks compare sides that differ by construction:
// every declared field check becomes a permanent check_added item on every
// adopt, for a constraint already in the database, while the same run emits
// "N declared CHECK constraint(s) ... not verified".
//
// Carrying is not comparing. DetectCheckConstraintDrift is what compares the
// two, by normalized expression — an author's `total > 0` against Postgres's
// `((total > 0))` — and unverifiedWarnings reports that adopt itself did not.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable \
//	  go test ./internal/introspect/ -run CheckCarry -v
func TestCheckCarryProducesNoFalseDrift(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to run the check-carry test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	exec(`DROP TABLE IF EXISTS public.cc`)
	exec(`CREATE TABLE public.cc (
	        id     int PRIMARY KEY,
	        total  int NOT NULL CONSTRAINT cc_total_check CHECK (total > 0))`)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS public.cc`) })

	const atl = `
entity Cc in pub {
  id    int primary
  total int not null check "total > 0"
  table "public.cc"
}
`
	f, err := dsl.Parse("cc.atl", []byte(atl))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	declared, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	live, _, warnings, err := FromPostgres(ctx, pool, declared)
	if err != nil {
		t.Fatalf("FromPostgres: %v", err)
	}

	d := codegen.ComputeDiff(live, declared)
	for _, c := range append(append(append([]codegen.Change{},
		d.Additive...), d.BackfillRequired...), d.Breaking...) {
		switch c.Kind {
		case codegen.KindCheckAdded, codegen.KindCheckRemoved, codegen.KindCheckChanged:
			t.Errorf("adopting a database that already has this CHECK reported %s "+
				"on %s.%s (%q).\n"+
				"  The constraint is present in the database and declared in the "+
				"schema; there is nothing to add. Introspection does not read CHECK "+
				"predicates, so any comparison here is between a declared value and "+
				"an empty one.",
				c.Kind, c.EntityID, c.Field, c.Constraint)
		}
	}

	// The warning is the mechanism that keeps carrying-across honest, so its
	// absence would mean the report silently claims a verification it did not do.
	var warned bool
	for _, w := range warnings {
		if len(w) > 0 && containsCheck(w) {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no CHECK-unverified warning in %v — carrying the declared value "+
			"across is only defensible while the report says it was not verified",
			warnings)
	}
}

func containsCheck(s string) bool {
	for i := 0; i+5 <= len(s); i++ {
		if s[i:i+5] == "CHECK" {
			return true
		}
	}
	return false
}
