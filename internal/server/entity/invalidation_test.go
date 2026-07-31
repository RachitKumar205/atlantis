package entity

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// `invalidate_on` declares that a write to one entity invalidates another. The
// `write(self)` half was already true in effect — every create/update/delete
// enqueues a self-invalidation unconditionally — so what was missing is the
// cross-entity fan-out: a write to CartItem invalidating the Cart it belongs to.
//
// The declaration is parent-centric and the lookup is child-centric, which is
// the one genuinely confusing part. Cart declares
// `write(CartItem where cart_id = self.id)`; the code that needs it runs on
// CartItem's write path and knows only "I am consumer.CartItem". Hence the
// inverted index.

func parentChildIR() *dsl.IR {
	return &dsl.IR{Entities: []dsl.Entity{
		{
			Name: "Cart", Namespace: "consumer",
			Fields: []dsl.Field{{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true}},
			Cache: &dsl.Cache{
				HasReadThrough: true, TTLMS: 1000,
				Invalidate: []dsl.Invalidate{
					{Self: true},
					{TargetID: "consumer.CartItem", Where: &dsl.InvalWhere{Field: "cart_id", SelfField: "id"}},
				},
			},
		},
		{
			Name: "CartItem", Namespace: "consumer",
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
				{Name: "cart_id", Type: dsl.FieldType{Name: "bigint"}, NotNull: true},
			},
		},
	}}
}

func TestInboundIndexIsKeyedByTheWrittenEntity(t *testing.T) {
	idx := buildInboundIndex(parentChildIR())

	// Keyed by the CHILD. Keying by the parent would be the natural reading of
	// the declaration and useless at the point of use.
	rules := idx["consumer.CartItem"]
	if len(rules) != 1 {
		t.Fatalf("consumer.CartItem has %d inbound rules, want 1: %+v", len(rules), rules)
	}
	if rules[0].parentEntityID != "consumer.Cart" {
		t.Errorf("parent = %q, want consumer.Cart", rules[0].parentEntityID)
	}
	if rules[0].childCol != "cart_id" {
		t.Errorf("childCol = %q, want cart_id — the column on the written row "+
			"that carries the parent's key", rules[0].childCol)
	}

	// `write(self)` must not produce an inbound rule. It is already handled
	// unconditionally on the write path, and a rule for it would enqueue the
	// same invalidation twice per write.
	if got := len(idx["consumer.Cart"]); got != 0 {
		t.Errorf("consumer.Cart has %d inbound rules, want 0 — write(self) is not "+
			"a cross-entity rule", got)
	}
}

// Self wins over a target when a checkpoint carries both.
//
// Lowering never produces that combination, so TargetID == "" alone covers
// every authored rule — but the IR is also read back from persisted JSON, where
// nothing enforces the invariant. Without this case the `inv.Self` half of the
// guard is unfalsifiable, and an unfalsifiable condition is a claim nothing
// checks.
func TestSelfWinsOverATargetOnTheSameRule(t *testing.T) {
	ir := parentChildIR()
	ir.Entities[0].Cache.Invalidate = []dsl.Invalidate{{
		Self:     true,
		TargetID: "consumer.CartItem",
		Where:    &dsl.InvalWhere{Field: "cart_id", SelfField: "id"},
	}}

	if got := buildInboundIndex(ir)["consumer.CartItem"]; len(got) != 0 {
		t.Errorf("a rule marked Self produced %d cross-entity rules; self is "+
			"already invalidated unconditionally, so this would enqueue twice "+
			"per write", len(got))
	}
}

// A rule naming an entity that is not in the IR must be dropped, not carried.
// It would enqueue outbox rows for an entity id nothing reads.
func TestInboundIndexDropsUnresolvableRules(t *testing.T) {
	ir := parentChildIR()
	ir.Entities[0].Cache.Invalidate = append(ir.Entities[0].Cache.Invalidate,
		dsl.Invalidate{TargetID: "consumer.Ghost", Where: &dsl.InvalWhere{Field: "x", SelfField: "id"}})

	if got := buildInboundIndex(ir)["consumer.Ghost"]; len(got) != 0 {
		t.Errorf("kept %d rules for an entity that does not exist", len(got))
	}
}

// A cross-entity rule with no where-mapping cannot identify which parent row to
// invalidate, and invalidating every row of the parent entity is not what was
// declared.
func TestInboundIndexDropsRulesWithNoMapping(t *testing.T) {
	ir := parentChildIR()
	ir.Entities[0].Cache.Invalidate = []dsl.Invalidate{{TargetID: "consumer.CartItem"}}

	if got := buildInboundIndex(ir)["consumer.CartItem"]; len(got) != 0 {
		t.Errorf("kept %d rules with no where-mapping", len(got))
	}
}

// The child's write SQL must return the parent key, or the write path has no
// way to learn it.
func TestWriteSQLReturnsTheParentKey(t *testing.T) {
	ir := parentChildIR()
	child := &ir.Entities[1]
	cols := inboundColumns(buildInboundIndex(ir)[child.ID()])
	if len(cols) != 1 || cols[0] != "cart_id" {
		t.Fatalf("inbound columns = %v, want [cart_id]", cols)
	}

	ins := buildInsertSQL(child, cols)
	if !strings.Contains(ins, `RETURNING "id", "cart_id"`) {
		t.Errorf("INSERT does not return the parent key:\n%s", ins)
	}
	upd := buildUpdateSQL(child, cols)
	if !strings.HasSuffix(upd, `RETURNING "cart_id"`) {
		t.Errorf("UPDATE does not return the parent key:\n%s", upd)
	}
	del := buildDeleteSQL(child, cols)
	if !strings.HasSuffix(del, `RETURNING "cart_id"`) {
		t.Errorf("DELETE does not return the parent key; the row is gone "+
			"afterwards and there is nothing left to read:\n%s", del)
	}

	// The pre-update read must lock. An unlocked read could observe a value
	// another transaction is mid-change on, and invalidate a cart this row was
	// never in.
	sel := buildSelectInboundSQL(child, cols)
	if !strings.Contains(sel, "FOR UPDATE") {
		t.Errorf("pre-update read does not lock the row:\n%s", sel)
	}
}

// The change must be invisible to entities with no inbound rules — which is
// almost all of them. This is the assertion that keeps a cache feature from
// silently reshaping every write in the system.
func TestWriteSQLIsUnchangedWithoutInboundRules(t *testing.T) {
	ir := parentChildIR()
	parent := &ir.Entities[0] // Cart: declares rules, receives none

	if got := buildInboundIndex(ir)[parent.ID()]; len(got) != 0 {
		t.Fatalf("fixture drifted: Cart should receive no inbound rules")
	}

	for name, pair := range map[string][2]string{
		"insert": {buildInsertSQL(parent, nil), buildInsertSQL(parent, inboundColumns(nil))},
		"update": {buildUpdateSQL(parent, nil), buildUpdateSQL(parent, inboundColumns(nil))},
		"delete": {buildDeleteSQL(parent, nil), buildDeleteSQL(parent, inboundColumns(nil))},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s SQL differs with an empty rule set:\n%s\n%s", name, pair[0], pair[1])
		}
		if strings.Count(pair[0], "RETURNING") > 1 {
			t.Errorf("%s SQL gained a second RETURNING clause:\n%s", name, pair[0])
		}
	}
	if sel := buildSelectInboundSQL(parent, nil); sel != "" {
		t.Errorf("an entity with no inbound rules got a pre-update read: %q", sel)
	}
	// UPDATE and DELETE must stay statements rather than becoming queries, so
	// the RowsAffected()==0 not-found path is untouched for these entities.
	if strings.Contains(buildUpdateSQL(parent, nil), "RETURNING") {
		t.Error("UPDATE became a query for an entity with no inbound rules")
	}
	if strings.Contains(buildDeleteSQL(parent, nil), "RETURNING") {
		t.Error("DELETE became a query for an entity with no inbound rules")
	}
}

// Column order fixes the RETURNING clause and therefore the scan order. Derived
// from a map, it would differ between runs and the scan would read the wrong
// value into the wrong slot.
func TestInboundColumnsAreDeterministic(t *testing.T) {
	rules := []inboundRule{
		{parentEntityID: "a.P", childCol: "zeta"},
		{parentEntityID: "a.Q", childCol: "alpha"},
		{parentEntityID: "a.R", childCol: "zeta"},
	}
	for i := 0; i < 50; i++ {
		got := inboundColumns(rules)
		if len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
			t.Fatalf("inboundColumns = %v, want [alpha zeta] deduped and sorted", got)
		}
	}
}

// An entity some procedure writes must not be served from the read cache.
//
// A procedure runs raw SQL and declares which entities it touches, but nothing
// reports which ROWS changed — and a row body is cached under a per-row version
// pointer, so there is no way to invalidate the bodies it invalidated. The
// generation bump it does emit clears cached query results only.
//
// This cost nothing before Get was served from the cache. It is live now: in
// the shipped schemas ten procedures write entities that declare read_through,
// so HardDeleteProduct would delete a product and leave its cached body being
// served until TTL.
func TestProcedureWrittenEntitiesAreNotCached(t *testing.T) {
	src := `
entity Widget in proc {
  id bigint primary
  label text
  cache { read_through ttl=5m }
}
entity Gadget in proc {
  id bigint primary
  label text
  cache { read_through ttl=5m }
}
procedure TouchWidget for Widget {
  input { id: bigint }
  steps {
    sql touches(proc.Widget) {
      UPDATE atlantis.proc_widget SET label = 'x' WHERE id = $id
    }
  }
}
`
	f, err := dsl.Parse("proc.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	written := procedureWrittenEntities(ir)
	if !written["proc.Widget"] {
		t.Fatal("proc.Widget is written by a procedure and was not detected")
	}
	if written["proc.Gadget"] {
		t.Fatal("proc.Gadget is written by no procedure and was flagged")
	}

	for i := range ir.Entities {
		e := &ir.Entities[i]
		meta := entityMetaFor(e, ir)
		switch e.Name {
		case "Widget":
			if meta.cacheable {
				t.Error("Widget is written by a procedure that cannot invalidate row " +
					"bodies, and is still cacheable — a Get after that procedure " +
					"would serve the pre-write row until TTL")
			}
		case "Gadget":
			if !meta.cacheable {
				t.Error("Gadget is written by no procedure and lost its cache; " +
					"failing closed must be targeted, not blanket")
			}
		}
	}
}

// A procedure writing a child must at least bump the PARENT's generation, so
// cached query results over the parent go. It cannot invalidate the parent's
// row bodies — it does not know which rows — but that is why such parents are
// themselves excluded from caching when a procedure writes them.
func TestProcedureBumpsParentsOfWrittenChildren(t *testing.T) {
	idx := buildInboundIndex(parentChildIR())

	got := parentsOf(idx, []string{"consumer.CartItem"})
	if len(got) != 1 || got[0] != "consumer.Cart" {
		t.Errorf("parentsOf(CartItem) = %v, want [consumer.Cart] — writing a cart "+
			"line changes what a query over Cart returns", got)
	}

	// An entity no rule names contributes no parents.
	if got := parentsOf(idx, []string{"consumer.Cart"}); len(got) != 0 {
		t.Errorf("parentsOf(Cart) = %v, want none", got)
	}

	// Deterministic: the result drives outbox writes.
	for i := 0; i < 20; i++ {
		if g := parentsOf(idx, []string{"consumer.CartItem"}); len(g) != 1 {
			t.Fatalf("unstable result: %v", g)
		}
	}
}

// When every inbound column is a primary-key column the pre-update read is
// skipped: PK columns are excluded from the UPDATE's SET list, so the value
// cannot change and the handler already holds it in pkValues.
//
// This is the shipped consumer.CartItem shape (primary by cart_id, variant_id).
// Without the skip, every cart-line update paid a round trip and a row lock to
// re-learn a value it had.
func TestNoPreReadWhenTheParentKeyIsPartOfThePK(t *testing.T) {
	e := &dsl.Entity{
		Name: "CartItem", Namespace: "consumer",
		Fields: []dsl.Field{
			{Name: "cart_id", Type: dsl.FieldType{Name: "bigint"}, NotNull: true},
			{Name: "variant_id", Type: dsl.FieldType{Name: "bigint"}, NotNull: true},
			{Name: "qty", Type: dsl.FieldType{Name: "int"}, NotNull: true},
		},
		CompositePK: []string{"cart_id", "variant_id"},
	}
	if got := buildSelectInboundSQL(e, []string{"cart_id"}); got != "" {
		t.Errorf("emitted a pre-update read for a PK column, which cannot change:\n%s", got)
	}

	// A non-PK parent key still needs it — that is the reparenting case.
	if got := buildSelectInboundSQL(e, []string{"qty"}); got == "" {
		t.Error("skipped the pre-update read for a mutable column; reparenting " +
			"would then invalidate only the parent the row joined")
	}
}
