package pg

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/coltype"
)

// Every type the registry marks as text-carried round-trips as a Go string
// through a pool this package built.
//
// Through a pool from New, not a connection with the codecs registered by
// hand: a type added to the registry and missed by registerTextCodecs fails
// here rather than at a customer's first SELECT.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@host:5432/atlantis \
//	    go test ./internal/storage/pg -run TextCodec -count=1
func TestTextCodecTypesRoundTripAsStrings(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the text codecs against a real database")
	}

	ctx := context.Background()
	p, err := New(ctx, Config{
		URL:               dsn,
		MaxConns:          2,
		MinConns:          1,
		MaxConnIdleTime:   time.Minute,
		MaxConnLifetime:   time.Hour,
		HealthCheckPeriod: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	defer p.Close()

	// One literal per type, in the text form Postgres parses.
	lit := map[string]string{
		"inet": "10.0.0.1", "cidr": "10.0.0.0/8",
		"bit": "10101010", "varbit": "1010", "tsvector": "a b",
		"int4range": "[1,5)", "int8range": "[1,5)", "numrange": "[1.5,5.5)",
		"tsrange":   "[2024-01-01,2024-02-01)",
		"tstzrange": "[2024-01-01,2024-02-01)",
		"daterange": "[2024-01-01,2024-02-01)",
		"point":     "(1,2)", "line": "{1,2,3}", "lseg": "[(0,0),(1,1)]",
		"box": "((0,0),(1,1))", "path": "((0,0),(1,1))",
		"polygon": "((0,0),(1,1),(2,0))", "circle": "<(0,0),1>",
	}
	// The declared column type, where the .atl spelling is not the DDL one.
	ddl := map[string]string{"bit": "bit(8)", "varbit": "bit varying"}

	names := coltype.TextCodecTypes()
	if len(names) == 0 {
		t.Fatal("no types are marked text-carried, so this test proves nothing")
	}

	for i, name := range names {
		t.Run(name, func(t *testing.T) {
			value, ok := lit[name]
			if !ok {
				t.Fatalf("%s is marked text-carried but has no literal here, so "+
					"nothing checks that a pool can carry it", name)
			}
			pgType := name
			if d, ok := ddl[name]; ok {
				pgType = d
			}

			tx, err := p.BeginTx(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			// Rolled back, so the database is unchanged.
			defer func() { _ = tx.Rollback(context.Background()) }()

			// A distinct name per type. One shared name makes the INSERT
			// text identical across subtests, so pgx reuses a prepared
			// statement planned against the previous column type and
			// Postgres answers "cached plan must not change result type".
			tbl := fmt.Sprintf("textcodec_probe_%d", i)
			if _, err := tx.Exec(ctx, fmt.Sprintf(`CREATE TEMP TABLE %s (v %s)`, tbl, pgType)); err != nil {
				t.Fatalf("create: %v", err)
			}
			// No cast in the statement: the codec has to carry both directions,
			// because the emitted SQL contains none either.
			if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (v) VALUES ($1)`, tbl), value); err != nil {
				t.Fatalf("bind %q: %v", value, err)
			}

			var got string
			if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT v FROM %s`, tbl)).Scan(&got); err != nil {
				t.Fatalf("scan into string: %v", err)
			}
			if got == "" {
				t.Errorf("read back empty for a column written %q", value)
			}

			// The nullable path scans through sql.NullString, which is a
			// different decoder.
			var null sql.NullString
			if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT NULL::%s`, pgType)).Scan(&null); err != nil {
				t.Fatalf("scan NULL into sql.NullString: %v", err)
			}
			if null.Valid {
				t.Errorf("a NULL read back as valid %q", null.String)
			}
		})
	}
}
