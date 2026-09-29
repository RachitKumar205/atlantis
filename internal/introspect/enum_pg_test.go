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

// Introspection reports the enum types the database has, so a declared enum
// does not read as removed.
//
// Without this the live IR carries no enums at all and ComputeDiff reports
// KindEnumRemoved for every declared one — a destructive change, on a schema
// that matches.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@host:5432/atlantis \
//	    go test ./internal/introspect -run Enum -count=1
func TestIntrospectReadsEnumLabels(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to read enums from a real database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	drop := func() {
		for _, stmt := range []string{
			`DROP TABLE IF EXISTS atlantis.enumintro_person`,
			`DROP TYPE IF EXISTS atlantis.enumintro_mood`,
		} {
			if _, err := pool.Exec(context.Background(), stmt); err != nil {
				t.Errorf("clean up %q: %v", stmt, err)
			}
		}
	}
	drop()
	t.Cleanup(drop)

	for _, stmt := range []string{
		`CREATE SCHEMA IF NOT EXISTS atlantis`,
		`CREATE TYPE atlantis.enumintro_mood AS ENUM ('happy', 'sad', 'in progress')`,
		`CREATE TABLE atlantis.enumintro_person (
			id bigint PRIMARY KEY,
			mood atlantis.enumintro_mood NOT NULL
		)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	declared := &dsl.IR{
		Entities: []dsl.Entity{{
			Name:      "Person",
			Namespace: "enumintro",
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true},
				{Name: "mood", Type: dsl.FieldType{Name: "enumintro.Mood", Enum: true}, NotNull: true},
			},
		}},
		Enums: []dsl.Enum{{
			Name:      "Mood",
			Namespace: "enumintro",
			Values:    []string{"happy", "sad", "in progress"},
		}},
	}

	live, _, _, err := introspect.FromPostgres(ctx, pool, declared)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if len(live.Enums) != 1 {
		t.Fatalf("live IR carries %d enums, want 1 — a declared enum would read as removed", len(live.Enums))
	}
	if got, want := live.Enums[0].ID(), "enumintro.Mood"; got != want {
		t.Errorf("enum ID = %q, want %q", got, want)
	}
	if got, want := strings.Join(live.Enums[0].Values, "|"), "happy|sad|in progress"; got != want {
		t.Errorf("labels = %q, want %q", got, want)
	}
}

// A declared enum with no type in the database is absent from the live IR, so
// the diff reports it as added rather than as a mismatch.
func TestIntrospectOmitsAnEnumThatDoesNotExist(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to read enums from a real database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	declared := &dsl.IR{
		Enums: []dsl.Enum{{
			Name:      "Nowhere",
			Namespace: "enumintro",
			Values:    []string{"a"},
		}},
	}
	live, _, _, err := introspect.FromPostgres(ctx, pool, declared)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if len(live.Enums) != 0 {
		t.Errorf("live IR carries %d enums for a type that does not exist", len(live.Enums))
	}
}
