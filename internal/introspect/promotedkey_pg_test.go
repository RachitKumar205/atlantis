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

// A table with no PRIMARY KEY but a NOT NULL UNIQUE column is addressable by
// that column, and is read that way.
//
// PRIMARY KEY is UNIQUE and NOT NULL together. Postgres refuses a foreign key
// to a table with no unique constraint, so every table a foreign key points at
// has such a column — and skipping those tables for "no primary key" drops the
// foreign keys that reference them too.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@host:5432/atlantis \
//	    go test ./internal/introspect -run PromotedKey -count=1
func TestPromotedKeyPG(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to read constraints from a real database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	drop := func() { _, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS promokey CASCADE`) }
	drop()
	t.Cleanup(drop)

	for _, stmt := range []string{
		`CREATE SCHEMA promokey`,
		// No PRIMARY KEY. This is the shape a foreign key can point at.
		`CREATE TABLE promokey.providers (id bigint NOT NULL UNIQUE, name text)`,
		// A real primary key is preferred over any unique constraint.
		`CREATE TABLE promokey.both (id bigint PRIMARY KEY, code text NOT NULL UNIQUE)`,
		// UNIQUE but nullable: two rows may hold NULL, so it addresses nothing.
		`CREATE TABLE promokey.nullable_uniq (id bigint UNIQUE, name text)`,
		// Unique only across a pair; neither column addresses a row alone.
		`CREATE TABLE promokey.composite (a bigint NOT NULL, b bigint NOT NULL, UNIQUE (a, b))`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	read := func(entity, table string, cols ...string) (*dsl.Entity, []string) {
		t.Helper()
		fields := make([]dsl.Field, len(cols))
		for i, c := range cols {
			fields[i] = dsl.Field{Name: c, Type: dsl.FieldType{Name: "bigint"}}
		}
		declared := &dsl.IR{Entities: []dsl.Entity{{
			Name: entity, Namespace: "promokey", TableName: table, Fields: fields,
		}}}
		live, _, warns, err := introspect.FromPostgres(ctx, pool, declared)
		if err != nil {
			t.Fatalf("introspect %s: %v", table, err)
		}
		return &live.Entities[0], warns
	}

	t.Run("a NOT NULL unique becomes the key", func(t *testing.T) {
		e, warns := read("Providers", "promokey.providers", "id", "name")
		if pf := e.PrimaryField(); pf == nil || pf.Name != "id" {
			t.Fatalf("primary field = %v, want id — the table is skipped for having no key", pf)
		}
		var named bool
		for _, w := range warns {
			if strings.Contains(w, "promokey.Providers") && strings.Contains(w, "NOT NULL and UNIQUE") {
				named = true
			}
		}
		if !named {
			t.Errorf("the promotion was not reported; the declaration claims a "+
				"primary key the catalogue does not have. warnings: %v", warns)
		}
	})

	t.Run("a real primary key wins", func(t *testing.T) {
		e, _ := read("Both", "promokey.both", "id", "code")
		if pf := e.PrimaryField(); pf == nil || pf.Name != "id" {
			t.Errorf("primary field = %v, want the declared primary key id", pf)
		}
	})

	t.Run("a nullable unique is not a key", func(t *testing.T) {
		e, _ := read("NullableUniq", "promokey.nullable_uniq", "id", "name")
		if pf := e.PrimaryField(); pf != nil {
			t.Errorf("primary field = %q; two rows may hold NULL there, so it "+
				"addresses nothing", pf.Name)
		}
	})

	t.Run("a composite unique is not a single-column key", func(t *testing.T) {
		e, _ := read("Composite", "promokey.composite", "a", "b")
		if pf := e.PrimaryField(); pf != nil {
			t.Errorf("primary field = %q; neither column is unique on its own", pf.Name)
		}
	})
}
