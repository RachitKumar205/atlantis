package introspect_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/introspect"
)

// `serial` is a sequence AND a NOT NULL, so only a NOT NULL column gets it.
//
// Postgres's SERIAL expands to an integer, a sequence default and NOT NULL;
// lowering reads the keyword back as all three. A hand-written
// `bigint DEFAULT nextval(...)` has the sequence and not the NOT NULL, so
// calling it `serial` declares a constraint the database does not have and the
// next plan proposes tightening the column.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@host:5432/atlantis \
//	    go test ./internal/introspect -run Serial -count=1
func TestANullableSequenceDefaultIsNotSerial(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to read a sequence default from a real database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	drop := func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS serialprobe CASCADE`)
	}
	drop()
	t.Cleanup(drop)

	for _, stmt := range []string{
		`CREATE SCHEMA serialprobe`,
		`CREATE SEQUENCE serialprobe.loose_seq`,
		// Nullable, with a sequence default. Ordinary in a legacy schema and
		// not what SERIAL means.
		`CREATE TABLE serialprobe.loose (
			id   bigint DEFAULT nextval('serialprobe.loose_seq'),
			name text NOT NULL
		)`,
		// The real thing, which carries the NOT NULL.
		`CREATE TABLE serialprobe.tight (
			id   bigserial PRIMARY KEY,
			name text NOT NULL
		)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	read := func(entity, table string) *dsl.Field {
		t.Helper()
		declared := &dsl.IR{Entities: []dsl.Entity{{
			Name:      entity,
			Namespace: "serialprobe",
			TableName: table,
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}},
				{Name: "name", Type: dsl.FieldType{Name: "text"}, NotNull: true},
			},
		}}}
		live, _, _, err := introspect.FromPostgres(ctx, pool, declared)
		if err != nil {
			t.Fatalf("introspect %s: %v", table, err)
		}
		f := live.Entities[0].FindField("id")
		if f == nil {
			t.Fatalf("no id field on %s", table)
		}
		return f
	}

	loose := read("Loose", "serialprobe.loose")
	if loose.Serial {
		t.Error("a nullable column with a sequence default was read as `serial`, " +
			"which implies a NOT NULL the database does not have")
	}
	if loose.NotNull {
		t.Error("a nullable column was read as NOT NULL")
	}
	if loose.Default == nil {
		t.Error("the sequence default was dropped; the declaration would propose removing it")
	} else if !strings.Contains(strings.ToLower(loose.Default.Str), "nextval") {
		t.Errorf("default = %q, want one naming nextval", loose.Default.Str)
	}

	tight := read("Tight", "serialprobe.tight")
	if !tight.Serial {
		t.Error("a bigserial column was not read as `serial`")
	}
	if !tight.NotNull {
		t.Error("a bigserial column was not read as NOT NULL")
	}
}
