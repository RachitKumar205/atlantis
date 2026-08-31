package codegen

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// The emitted DDL has to be DDL Postgres accepts, and the columns it creates
// have to be the types the declaration named.
//
// TestNewScalarTypesReachDDL matches strings; this runs them. A spelling the
// registry renders wrongly is string-clean either way — `DOUBLE` for double
// precision parses as a column name — and fails here.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@host:5432/atlantis \
//	    go test ./internal/codegen -run PG -count=1
func TestNewScalarTypesApplyToPostgres(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to run the emitted DDL against a real database")
	}

	src := "entity Legacy in probe {\n" +
		"  id bigint primary\n" +
		"  code char(10)\n" +
		"  label char\n" +
		"  seen_at timestamp\n" +
		"  wallclock time\n" +
		"  zoned timetz\n" +
		"  payload json\n" +
		"  doc xml\n" +
		"  mac macaddr\n" +
		"  mac8 macaddr8\n" +
		"  price money\n" +
		"  ident name\n" +
		"  q tsquery\n" +
		"}\n"

	f, err := dsl.Parse("t.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	scripts, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	// Rolled back, so the database is unchanged whether or not the DDL is
	// valid.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, scripts.Up); err != nil {
		t.Fatalf("Postgres refused the emitted DDL: %v\n%s", err, scripts.Up)
	}

	// format_type is Postgres's own deparse, which is what columndrift
	// compares, so this pins the declaration against the shape drift will see.
	want := map[string]string{
		"code":      "character(10)",
		"label":     "character(1)",
		"seen_at":   "timestamp without time zone",
		"wallclock": "time without time zone",
		"zoned":     "time with time zone",
		"payload":   "json",
		"doc":       "xml",
		"mac":       "macaddr",
		"mac8":      "macaddr8",
		"price":     "money",
		"ident":     "name",
		"q":         "tsquery",
	}
	rows, err := tx.Query(ctx, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod)
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		 WHERE c.relname = 'probe_legacy'
		   AND a.attnum > 0 AND NOT a.attisdropped`)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var name, ft string
		if err := rows.Scan(&name, &ft); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = ft
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	for col, wantType := range want {
		if got[col] != wantType {
			t.Errorf("%s is %q in Postgres, the declaration named %q", col, got[col], wantType)
		}
	}
}

// A sequence on a narrow integer round-trips: the declaration says `serial` on
// an int, the DDL says SERIAL, and Postgres reports the column as `integer`
// with a nextval default — which is what introspection reads back.
//
// A sequence with no .atl spelling is dropped from the declaration, and the
// next plan reports the column's default as removed. On the EBI dataset that
// shape was 32 of 53 drift items.
func TestNarrowSerialsApplyToPostgres(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to run the emitted DDL against a real database")
	}

	src := "entity Counter in probe {\n" +
		"  id int primary serial\n" +
		"  small smallint serial\n" +
		"  wide bigint serial\n" +
		"}\n"

	f, err := dsl.Parse("t.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	scripts, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, scripts.Up); err != nil {
		t.Fatalf("Postgres refused the emitted DDL: %v\n%s", err, scripts.Up)
	}

	want := map[string][2]string{
		"id":    {"integer", "nextval"},
		"small": {"smallint", "nextval"},
		"wide":  {"bigint", "nextval"},
	}
	rows, err := tx.Query(ctx, `
		SELECT a.attname,
		       format_type(a.atttypid, a.atttypmod),
		       COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		  LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		 WHERE c.relname = 'probe_counter'
		   AND a.attnum > 0 AND NOT a.attisdropped`)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var name, ft, def string
		if err := rows.Scan(&name, &ft, &def); err != nil {
			t.Fatalf("scan: %v", err)
		}
		w, ok := want[name]
		if !ok {
			continue
		}
		seen++
		if ft != w[0] {
			t.Errorf("%s is %q, want %q", name, ft, w[0])
		}
		if !strings.Contains(def, w[1]) {
			t.Errorf("%s default is %q, want one containing %q", name, def, w[1])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen != len(want) {
		t.Errorf("found %d of the %d declared columns", seen, len(want))
	}
}

// The text-carried types reach Postgres as the types the declaration named.
//
// These are the ones pgx cannot decode into a Go string without a codec; the
// codec is registered by the pool and tested in internal/storage/pg. What is
// checked here is the other half — that the spelling the registry renders is
// the column Postgres creates.
func TestTextCarriedTypesApplyToPostgres(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to run the emitted DDL against a real database")
	}

	src := "entity Shapes in probe {\n" +
		"  id bigint primary\n" +
		"  addr inet\n" +
		"  net cidr\n" +
		"  flags bit(8)\n" +
		"  bits varbit\n" +
		"  doc tsvector\n" +
		"  span int4range\n" +
		"  wide int8range\n" +
		"  amounts numrange\n" +
		"  window tsrange\n" +
		"  zoned tstzrange\n" +
		"  days daterange\n" +
		"  spot point\n" +
		"  ray line\n" +
		"  seg lseg\n" +
		"  area box\n" +
		"  route path\n" +
		"  region polygon\n" +
		"  disc circle\n" +
		"}\n"

	f, err := dsl.Parse("t.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	scripts, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, scripts.Up); err != nil {
		t.Fatalf("Postgres refused the emitted DDL: %v\n%s", err, scripts.Up)
	}

	want := map[string]string{
		"addr": "inet", "net": "cidr",
		"flags": "bit(8)", "bits": "bit varying", "doc": "tsvector",
		"span": "int4range", "wide": "int8range", "amounts": "numrange",
		"window": "tsrange", "zoned": "tstzrange", "days": "daterange",
		"spot": "point", "ray": "line", "seg": "lseg", "area": "box",
		"route": "path", "region": "polygon", "disc": "circle",
	}
	rows, err := tx.Query(ctx, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod)
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		 WHERE c.relname = 'probe_shapes'
		   AND a.attnum > 0 AND NOT a.attisdropped`)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var name, ft string
		if err := rows.Scan(&name, &ft); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = ft
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	for col, wantType := range want {
		if got[col] != wantType {
			t.Errorf("%s is %q in Postgres, the declaration named %q", col, got[col], wantType)
		}
	}
}
