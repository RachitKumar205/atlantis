package introspect_test

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/introspect"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// Each entity field is in exactly one of these three, and the test below fails
// if dsl.Entity gains one that is in none.
//
// carried: copied from the declaration, because the catalogue cannot confirm it.
// derived: read from the catalogue, so a declared value is expected not to
// survive. dropped: neither, and a known defect rather than a decision.
var (
	carried = []string{
		"Name", "Namespace", "Kind", "TableName", "TimeField",
		"SoftDeleteField", "TouchOnUpdateField", "QueryTimeoutMS", "Cache",
		"Relations", "Indexes", "Uniques", "Checks", "TtlField",
		"RetiredProtoNumbers", "Keyless",
	}
	derived = []string{"Fields", "CompositePK", "PartitionField"}
	dropped = []string{"ChunkTimeIntervalMS"}
)

// privatePool opens a pool on a database of this test's own, dropped with the
// test.
//
// A schema created in the shared test database is visible to every other
// package's tests while it exists, and adopt's GenerateAll reads every schema
// the database has: a CREATE here and a DROP there race, and adopt fails with
// `could not open relation with OID` or with a schema it cannot find.
func privatePool(t *testing.T, dbName string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to read constraints from a real database")
	}
	dsn := pgcatalog.PrivateDatabase(t, adminDSN, dbName)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// Every field in `carried` is in dsl.Entity, and every field of dsl.Entity is
// accounted for.
//
// A field added to dsl.Entity and left out of FromPostgres's literal is deleted
// from the schema of record on every adopt, which is how `keyless`, `ttl_field`
// and the retired proto numbers were lost. Naming the three does not stop a
// fourth, so every field has to be classified.
func TestEveryEntityFieldIsClassified(t *testing.T) {
	rt := reflect.TypeOf(dsl.Entity{})
	real := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		real[rt.Field(i).Name] = true
	}

	seen := map[string]string{}
	for _, list := range []struct {
		what  string
		names []string
	}{{"carried", carried}, {"derived", derived}, {"dropped", dropped}} {
		for _, n := range list.names {
			if !real[n] {
				t.Errorf("%s lists %s, which dsl.Entity does not have", list.what, n)
				continue
			}
			if prior, dup := seen[n]; dup {
				t.Errorf("%s is in both %s and %s", n, prior, list.what)
				continue
			}
			seen[n] = list.what
			delete(real, n)
		}
	}
	for n := range real {
		t.Errorf("dsl.Entity.%s is classified nowhere, so nothing says whether "+
			"introspection should keep it", n)
	}
}

// Every carried declaration survives introspection with its value.
//
// Each is set to something the zero value would not produce, so dropping any
// one of them from the literal fails here. A name-only check passed while
// `soft_delete by` and the declared table override were silently deleted.
func TestCarriedDeclarationsSurvivePG(t *testing.T) {
	pool, ctx := privatePool(t, "atlantis_introspect_carryall")

	for _, stmt := range []string{
		`CREATE SCHEMA carryall`,
		`CREATE TABLE carryall.events (
		   id          bigint PRIMARY KEY,
		   tenant      text NOT NULL,
		   occurred_at timestamptz NOT NULL,
		   deleted_at  timestamptz,
		   touched_at  timestamptz,
		   body        text)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}

	declared := dsl.Entity{
		Name:      "Events",
		Namespace: "carryall",
		TableName: "carryall.events",
		Kind:      dsl.EntityKindRegular,
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, NotNull: true, Primary: true, ProtoNumber: 1},
			{Name: "tenant", Type: dsl.FieldType{Name: "text"}, NotNull: true, ProtoNumber: 2},
			{Name: "occurred_at", Type: dsl.FieldType{Name: "timestamptz"}, NotNull: true, ProtoNumber: 3},
			{Name: "deleted_at", Type: dsl.FieldType{Name: "timestamptz"}, ProtoNumber: 4},
			{Name: "touched_at", Type: dsl.FieldType{Name: "timestamptz"}, ProtoNumber: 5},
			{Name: "body", Type: dsl.FieldType{Name: "text"}, ProtoNumber: 6},
		},

		TimeField:           "occurred_at",
		SoftDeleteField:     "deleted_at",
		TouchOnUpdateField:  "touched_at",
		QueryTimeoutMS:      1234,
		TtlField:            "occurred_at",
		RetiredProtoNumbers: []int{7, 9},
		Cache:               &dsl.Cache{HasReadThrough: true, TTLMS: 600_000, Tag: "events:{id}"},
		Relations:           []dsl.Relation{{Name: "items", TargetID: "carryall.Item", Via: "event_id"}},
		Indexes:             []dsl.Index{{Field: "body"}},
		Uniques:             []dsl.UniqueSpec{{Fields: []string{"tenant", "body"}}},
		Checks:              []dsl.TableCheck{{Name: "body_present", Expr: "body IS NOT NULL"}},
	}

	got, _, _, err := introspect.FromPostgres(ctx, pool, &dsl.IR{
		Version: 1, Entities: []dsl.Entity{declared},
	})
	if err != nil {
		t.Fatalf("FromPostgres: %v", err)
	}
	if len(got.Entities) != 1 {
		t.Fatalf("introspection returned %d entities, want 1", len(got.Entities))
	}
	out := got.Entities[0]

	checks := []struct {
		field string
		ok    bool
		got   any
	}{
		{"Name", out.Name == "Events", out.Name},
		{"Namespace", out.Namespace == "carryall", out.Namespace},
		{"TableName", out.TableName == "carryall.events", out.TableName},
		{"Kind", out.Kind == dsl.EntityKindRegular, out.Kind},
		{"TimeField", out.TimeField == "occurred_at", out.TimeField},
		{"SoftDeleteField", out.SoftDeleteField == "deleted_at", out.SoftDeleteField},
		{"TouchOnUpdateField", out.TouchOnUpdateField == "touched_at", out.TouchOnUpdateField},
		{"QueryTimeoutMS", out.QueryTimeoutMS == 1234, out.QueryTimeoutMS},
		{"TtlField", out.TtlField == "occurred_at", out.TtlField},
		{"RetiredProtoNumbers", len(out.RetiredProtoNumbers) == 2, out.RetiredProtoNumbers},
		{"Cache", out.Cache != nil && out.Cache.Tag == "events:{id}", out.Cache},
		{"Relations", len(out.Relations) == 1, out.Relations},
		{"Indexes", len(out.Indexes) == 1, out.Indexes},
		{"Uniques", len(out.Uniques) == 1, out.Uniques},
		{"Checks", len(out.Checks) == 1, out.Checks},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s was dropped or changed, got %v", c.field, c.got)
		}
	}

	// Kind is asserted above and cannot fail on this fixture: EntityKindRegular
	// is the zero value, so dropping it from the literal is invisible here. A
	// hypertable fixture would be needed, and #126 adds one.

	// The table carries no policy, so the declared value would be reported as
	// absent. Asserting it keeps the derived fields honest beside the carried
	// ones.
	if out.PartitionField != "" {
		t.Errorf("PartitionField is %q, but the table has no row-level security "+
			"policy and adopt reports the table rather than the declaration",
			out.PartitionField)
	}
}

// `keyless` survives, on a table that has no key.
//
// Separate from the test above because false is the zero value: on a keyed
// entity, dropping the carry changes nothing observable.
func TestKeylessSurvivesPG(t *testing.T) {
	pool, ctx := privatePool(t, "atlantis_introspect_keyless")

	for _, stmt := range []string{
		`CREATE SCHEMA keylesscarry`,
		// No key, and no unique constraint to promote to one.
		`CREATE TABLE keylesscarry.notes (a text NOT NULL, b text)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}

	declared := dsl.Entity{
		Name: "Notes", Namespace: "keylesscarry",
		TableName: "keylesscarry.notes",
		Kind:      dsl.EntityKindRegular,
		Keyless:   true,
		Fields: []dsl.Field{
			{Name: "a", Type: dsl.FieldType{Name: "text"}, NotNull: true, ProtoNumber: 1},
			{Name: "b", Type: dsl.FieldType{Name: "text"}, ProtoNumber: 2},
		},
	}

	got, _, _, err := introspect.FromPostgres(ctx, pool, &dsl.IR{
		Version: 1, Entities: []dsl.Entity{declared},
	})
	if err != nil {
		t.Fatalf("FromPostgres: %v", err)
	}
	out := got.Entities[0]

	if !out.Keyless {
		t.Error("the declaration said `keyless` and the checkpoint lost it, which " +
			"is the entity the descriptor builder refuses")
	}
}
