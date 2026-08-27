package adopt

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
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
