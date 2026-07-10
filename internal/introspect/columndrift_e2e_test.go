package introspect

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// TestDetectColumnTypeDrift_EndToEnd reproduces the vendor_cart_id shape: the
// live column is varchar(10) while the .atl declares varchar(255). The
// detector must report the width drift, report nothing when they match, and
// not false-positive on serial/timestamptz/numeric canonicalization.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:pw@localhost:55432/atlantis?sslmode=disable \
//	  go test ./internal/introspect/ -run ColumnTypeDrift_EndToEnd -v
func TestDetectColumnTypeDrift_EndToEnd(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to run the column-drift e2e test")
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

	// Live table: vc is varchar(10); a bigserial id and a timestamptz to
	// prove serial/timestamptz canonicalization doesn't false-positive.
	exec(`DROP TABLE IF EXISTS public.cd`)
	exec(`CREATE TABLE public.cd (
		id bigserial PRIMARY KEY,
		vc varchar(10) NOT NULL,
		created_at timestamptz NOT NULL DEFAULT now(),
		amount numeric(10,2))`)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS public.cd`) })

	matchIR := lower(`entity Cd in x {
  table "public.cd"
  id         bigint primary serial
  vc         varchar(10) not null
  created_at timestamptz not null default now()
  amount     numeric(10, 2)
}`)
	driftIR := lower(`entity Cd in x {
  table "public.cd"
  id         bigint primary serial
  vc         varchar(255) not null
  created_at timestamptz not null default now()
  amount     numeric(10, 2)
}`)

	t.Run("declared equals live -> no drift", func(t *testing.T) {
		drift, notes, err := DetectColumnTypeDrift(ctx, pool, matchIR)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if len(drift) != 0 {
			t.Errorf("matching types should report no drift (no serial/ts false-positive), got %+v (notes %v)", drift, notes)
		}
	})

	t.Run("widened varchar -> drift", func(t *testing.T) {
		drift, _, err := DetectColumnTypeDrift(ctx, pool, driftIR)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if len(drift) != 1 {
			t.Fatalf("expected exactly 1 drift (vc), got %+v", drift)
		}
		d := drift[0]
		if d.Column != "vc" || d.Declared != "character varying(255)" || d.Live != "character varying(10)" {
			t.Errorf("unexpected drift: %+v", d)
		}
	})
}
