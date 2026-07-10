package introspect

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// TestDetectCheckConstraintDrift_EndToEnd reproduces the carts outage shape
// against a live Postgres: the live table enforces a NARROWER status check
// than the .atl declares (live allows 3 values, declared 4). The detector
// must (a) report no drift when declared == live, and (b) surface both
// directions when the .atl widened the value set the live constraint still
// rejects.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:pw@localhost:55432/atlantis?sslmode=disable \
//	  go test ./internal/introspect/ -run CheckConstraintDrift_EndToEnd -v
func TestDetectCheckConstraintDrift_EndToEnd(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to run the check-drift e2e test")
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
	lower := func(atl string) *dsl.IR {
		t.Helper()
		f, err := dsl.Parse("e2e.atl", []byte(atl))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		ir, err := dsl.Lower([]*dsl.File{f})
		if err != nil {
			t.Fatalf("lower: %v", err)
		}
		return ir
	}

	// Live table whose status check allows only 3 values — the pre-adopt
	// constraint the .atl later widened.
	exec(`DROP TABLE IF EXISTS public.ct`)
	exec(`CREATE TABLE public.ct (id int PRIMARY KEY, status varchar(20) NOT NULL)`)
	exec(`ALTER TABLE public.ct ADD CONSTRAINT ct_status_check
	      CHECK (status IN ('active','abandoned','checked_out'))`)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS public.ct`) })

	declaredMatch := lower(`entity Ct in x {
  table "public.ct"
  id     int primary
  status varchar(20) not null check "status IN ('active', 'abandoned', 'checked_out')"
}`)
	declaredWiden := lower(`entity Ct in x {
  table "public.ct"
  id     int primary
  status varchar(20) not null check "status IN ('active', 'awaiting_checkout', 'abandoned', 'checked_out')"
}`)

	t.Run("declared equals live -> no drift", func(t *testing.T) {
		drift, notes, err := DetectCheckConstraintDrift(ctx, pool, declaredMatch)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if len(drift) != 0 {
			t.Errorf("declared check equals live should report no drift, got %+v (notes %v)", drift, notes)
		}
	})

	t.Run("widened declared check -> drift both directions", func(t *testing.T) {
		drift, _, err := DetectCheckConstraintDrift(ctx, pool, declaredWiden)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		var declaredNotEnforced, liveNotDeclared int
		for _, d := range drift {
			switch d.Kind {
			case CheckDeclaredNotEnforced:
				declaredNotEnforced++
				if d.Table != "ct" {
					t.Errorf("unexpected table: %+v", d)
				}
			case CheckLiveNotDeclared:
				liveNotDeclared++
				if d.ConstraintName != "ct_status_check" {
					t.Errorf("expected live ct_status_check, got %+v", d)
				}
			}
		}
		if declaredNotEnforced != 1 || liveNotDeclared != 1 {
			t.Errorf("widen should report 1 declared_not_enforced + 1 live_not_declared, got %+v", drift)
		}
	})
}
