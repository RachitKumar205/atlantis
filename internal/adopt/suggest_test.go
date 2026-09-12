package adopt

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// Suggest is pure over the enriched IR, so these need no database. The value of
// a suggestion is that it fires on the schema that has the problem and stays
// quiet on the one that does not — a rule that always fires is one every reader
// learns to skip.

func entity(fields ...dsl.Field) *dsl.IR {
	return &dsl.IR{Entities: []dsl.Entity{{
		Name: "Thing", Namespace: "shop", Fields: fields,
	}}}
}

func kinds(s []Suggestion) map[string]Suggestion {
	out := make(map[string]Suggestion, len(s))
	for _, x := range s {
		out[x.Kind] = x
	}
	return out
}

func TestTenantIsolationIsSuggestedOnlyWhenAbsent(t *testing.T) {
	t.Run("a tenant column with no policy", func(t *testing.T) {
		ir := entity(
			dsl.Field{Name: "id", Primary: true},
			dsl.Field{Name: "tenant_id", NotNull: true},
		)
		got := kinds(Suggest(ir, nil))
		s, ok := got[SuggestTenantIsolation]
		if !ok {
			t.Fatal("a tenant_id column with no row-level security was not flagged")
		}
		if s.Line != "partition by tenant_id" {
			t.Errorf("Line = %q, want the .atl to paste", s.Line)
		}
	})

	t.Run("already isolated", func(t *testing.T) {
		ir := entity(
			dsl.Field{Name: "id", Primary: true},
			dsl.Field{Name: "tenant_id", NotNull: true},
		)
		// What loadPartitionPolicies sets when pg_policy already has one.
		ir.Entities[0].PartitionField = "tenant_id"
		if _, ok := kinds(Suggest(ir, nil))[SuggestTenantIsolation]; ok {
			t.Error("a table Postgres already isolates was told to isolate it")
		}
	})

	t.Run("a nullable tenant column says so", func(t *testing.T) {
		ir := entity(
			dsl.Field{Name: "id", Primary: true},
			dsl.Field{Name: "org_id"},
		)
		s := kinds(Suggest(ir, nil))[SuggestTenantIsolation]
		// A NULL never equals current_partition(), so those rows become
		// unreadable the moment the policy exists. Suggesting the line without
		// saying so proposes an outage.
		if !strings.Contains(s.Detail, "NOT NULL") {
			t.Errorf("a nullable tenant column was suggested without the NULL warning: %s", s.Detail)
		}
	})

	t.Run("an ordinary foreign key is left alone", func(t *testing.T) {
		ir := entity(
			dsl.Field{Name: "id", Primary: true},
			dsl.Field{Name: "customer_id", NotNull: true},
		)
		if _, ok := kinds(Suggest(ir, nil))[SuggestTenantIsolation]; ok {
			t.Error("customer_id was read as a tenant key; on most schemas it is a plain reference")
		}
	})
}

func TestMissingPrimaryKeyIsFlagged(t *testing.T) {
	ir := entity(dsl.Field{Name: "note"})
	if _, ok := kinds(Suggest(ir, nil))[SuggestNoPrimaryKey]; !ok {
		t.Error("a table with no primary key was not flagged")
	}

	withPK := entity(dsl.Field{Name: "id", Primary: true})
	if _, ok := kinds(Suggest(withPK, nil))[SuggestNoPrimaryKey]; ok {
		t.Error("a table with a primary key was flagged as having none")
	}

	composite := entity(dsl.Field{Name: "a"}, dsl.Field{Name: "b"})
	composite.Entities[0].CompositePK = []string{"a", "b"}
	if _, ok := kinds(Suggest(composite, nil))[SuggestNoPrimaryKey]; ok {
		t.Error("a composite primary key was not recognised as one")
	}
}

func TestUnindexedForeignKeyIsFlagged(t *testing.T) {
	ref := &dsl.Ref{}

	t.Run("no index", func(t *testing.T) {
		ir := entity(
			dsl.Field{Name: "id", Primary: true},
			dsl.Field{Name: "owner_id", Ref: ref},
		)
		s, ok := kinds(Suggest(ir, nil))[SuggestUnindexedForeignKey]
		if !ok {
			t.Fatal("a foreign key with no index was not flagged")
		}
		if s.Line != "index by owner_id" {
			t.Errorf("Line = %q, want the .atl to paste", s.Line)
		}
	})

	t.Run("an index leading on the column", func(t *testing.T) {
		ir := entity(
			dsl.Field{Name: "id", Primary: true},
			dsl.Field{Name: "owner_id", Ref: ref},
		)
		ir.Entities[0].Indexes = []dsl.Index{{
			Kind: dsl.IndexBtree, Fields: []dsl.IndexField{{Name: "owner_id"}},
		}}
		if _, ok := kinds(Suggest(ir, nil))[SuggestUnindexedForeignKey]; ok {
			t.Error("a foreign key that already has an index was flagged")
		}
	})

	t.Run("an index that does not lead on the column", func(t *testing.T) {
		ir := entity(
			dsl.Field{Name: "id", Primary: true},
			dsl.Field{Name: "owner_id", Ref: ref},
		)
		// (kind, owner_id) does not serve a lookup by owner_id alone, so
		// counting it would report the problem solved while it is not.
		ir.Entities[0].Indexes = []dsl.Index{{
			Kind:   dsl.IndexBtree,
			Fields: []dsl.IndexField{{Name: "kind"}, {Name: "owner_id"}},
		}}
		if _, ok := kinds(Suggest(ir, nil))[SuggestUnindexedForeignKey]; !ok {
			t.Error("a foreign key indexed only as a trailing column was treated as covered")
		}
	})
}

// adopt and the server agree on whether an entity has a key.
//
// A composite key naming fields the entity does not carry: `primary by` is
// non-empty, so counting the clause reports a key, while schema.PKColumns
// resolves each name and returns none. The server decides what publishes an
// API from PKColumns, so disagreement here means adopt stays quiet about a
// table that reaches the server with no key columns at all.
func TestAdoptAndTheServerAgreeOnWhatHasAKey(t *testing.T) {
	cases := []struct {
		what string
		want bool
		e    dsl.Entity
	}{
		{"a composite key naming no field the entity carries", false, dsl.Entity{
			Name: "Thing", Namespace: "shop",
			CompositePK: []string{"cart_id", "variant_id"},
			Fields:      []dsl.Field{{Name: "quantity"}},
		}},
		{"a composite key half of which resolves", true, dsl.Entity{
			Name: "Thing", Namespace: "shop",
			CompositePK: []string{"cart_id", "gone"},
			Fields:      []dsl.Field{{Name: "cart_id"}, {Name: "quantity"}},
		}},
		{"a resolvable composite key", true, dsl.Entity{
			Name: "Thing", Namespace: "shop",
			CompositePK: []string{"cart_id", "variant_id"},
			Fields:      []dsl.Field{{Name: "cart_id"}, {Name: "variant_id"}},
		}},
		{"a single primary field", true, dsl.Entity{
			Name: "Thing", Namespace: "shop",
			Fields: []dsl.Field{{Name: "id", Primary: true}},
		}},
		{"no key at all", false, dsl.Entity{
			Name: "Thing", Namespace: "shop",
			Fields: []dsl.Field{{Name: "notes"}},
		}},
	}

	// want is stated per case rather than read from PKColumns. hasPrimaryKey
	// delegates to PKColumns, so comparing the two compares the implementation
	// with itself and no change to either can fail it.
	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			e := c.e
			if got := hasPrimaryKey(&e); got != c.want {
				t.Errorf("hasPrimaryKey=%v, want %v", got, c.want)
			}
			if server := len(schema.PKColumns(&e)) > 0; server != c.want {
				t.Errorf("the server's PKColumns says %v, want %v", server, c.want)
			}
		})
	}
}

// The no-primary-key suggestion fires on a composite key that resolves to
// nothing.
//
// The suggestion is the only warning adopt gives about a table whose rows
// cannot be addressed, and this is the shape that reached production with none.
func TestAnUnresolvableCompositeKeyIsReportedAsNoPrimaryKey(t *testing.T) {
	ir := &dsl.IR{Entities: []dsl.Entity{{
		Name: "Thing", Namespace: "shop",
		CompositePK: []string{"cart_id", "variant_id"},
		Fields:      []dsl.Field{{Name: "quantity"}},
	}}}
	if _, ok := kinds(Suggest(ir, nil))[SuggestNoPrimaryKey]; !ok {
		t.Error("a composite key naming no existing field was not reported as having no primary key")
	}
}

// An entity adopt declares `keyless` carries no key beside the flag.
//
// `keyless` and a primary key are alternatives, and validateEntity refuses the
// pair, so a checkpoint holding one fails to decode. A `primary by` naming
// fields no column resolves to is the way both end up set: hasPrimaryKey reports
// no key, and the clause it read stays on the entity.
func TestAKeylessEntityAdoptDeclaresCarriesNoKey(t *testing.T) {
	ir := &dsl.IR{Entities: []dsl.Entity{{
		Name: "Thing", Namespace: "shop",
		// Neither name resolves, so this is a clause with no key behind it.
		CompositePK: []string{"cart_id", "variant_id"},
		Fields: []dsl.Field{
			{Name: "quantity", Type: dsl.FieldType{Name: "int"}},
		},
	}}}
	nameFor := map[string]string{"shop.Thing": "things"}

	dropUndeclarableEntities(ir, nameFor)

	if len(ir.Entities) != 1 {
		t.Fatalf("the entity was dropped, so this proves nothing: %d left", len(ir.Entities))
	}
	e := ir.Entities[0]
	if !e.Keyless {
		t.Error("an entity with no resolvable key was not declared keyless")
	}
	if len(e.CompositePK) != 0 {
		t.Errorf("keyless was set beside a primary key %v, which validateEntity refuses",
			e.CompositePK)
	}
}
