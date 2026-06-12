package codegen

import (
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

const procOld = customSchemaFixture + `
procedure DeleteOutfit for SavedOutfit {
  input { outfit_id: bigint }
  steps {
    update SavedOutfit set deleted_at = now() where id = $outfit_id
  }
}
`

// Same procedure name/owner, different step body (WHERE column changed).
const procChanged = customSchemaFixture + `
procedure DeleteOutfit for SavedOutfit {
  input { outfit_id: bigint }
  steps {
    update SavedOutfit set deleted_at = now() where consumer_id = $outfit_id
  }
}
`

func TestDiff_ProcedureAdded_IsVisibleAdditive(t *testing.T) {
	oldIR := lowerCustom(t, customSchemaFixture)
	newIR := lowerCustom(t, procOld)
	d := ComputeDiff(oldIR, newIR)
	if d.IsEmpty() {
		t.Fatal("adding a procedure must not report as zero changes")
	}
	c := findChange(t, d, KindProcedureAdded)
	if c == nil || c.Class != ClassAdditive {
		t.Fatalf("want additive procedure_added, got %+v", c)
	}
	if c.EntityID != "consumer.DeleteOutfit" {
		t.Errorf("EntityID = %q, want consumer.DeleteOutfit", c.EntityID)
	}
}

func TestDiff_ProcedureChanged_IsVisible(t *testing.T) {
	oldIR := lowerCustom(t, procOld)
	newIR := lowerCustom(t, procChanged)
	d := ComputeDiff(oldIR, newIR)
	if c := findChange(t, d, KindProcedureChanged); c == nil {
		t.Fatalf("changing a procedure body must surface procedure_changed, got diff %+v", d)
	}
	// A pure body edit must NOT also report add/remove.
	if findChange(t, d, KindProcedureAdded) != nil || findChange(t, d, KindProcedureRemoved) != nil {
		t.Errorf("body edit should be a single 'changed', got %+v", d)
	}
}

func TestDiff_ProcedureRemoved_IsVisible(t *testing.T) {
	oldIR := lowerCustom(t, procOld)
	newIR := lowerCustom(t, customSchemaFixture)
	d := ComputeDiff(oldIR, newIR)
	if c := findChange(t, d, KindProcedureRemoved); c == nil || c.Class != ClassAdditive {
		t.Fatalf("want procedure_removed additive, got %+v", c)
	}
}

func TestDiff_ProcedureUnchanged_NoDiff(t *testing.T) {
	oldIR := lowerCustom(t, procOld)
	newIR := lowerCustom(t, procOld)
	d := ComputeDiff(oldIR, newIR)
	if !d.IsEmpty() {
		t.Errorf("identical procedures should produce no diff, got %+v", d)
	}
}

func TestDiff_CustomQueryAdded_IsVisible(t *testing.T) {
	oldIR := lowerCustom(t, customSchemaFixture)
	newIR := lowerCustom(t, customSchemaFixture+`
query OutfitsForConsumer for SavedOutfit {
  input { consumer_id: bigint }
  output as SavedOutfit
  sql touches(SavedOutfit) {
    SELECT id, consumer_id, name FROM consumer_saved_outfit WHERE consumer_id = $consumer_id
  }
}
`)
	d := ComputeDiff(oldIR, newIR)
	if c := findChange(t, d, KindCustomQueryAdded); c == nil || c.EntityID != "consumer.OutfitsForConsumer" {
		t.Fatalf("want custom_query_added for consumer.OutfitsForConsumer, got %+v", c)
	}
}

// A file move (SourcePath differs) is not a content change.
func TestCustomProcContentEqual_IgnoresSourcePath(t *testing.T) {
	a := &dsl.CustomProcedure{Name: "P", Owner: "ns.E", SourcePath: "a/x.atl"}
	b := &dsl.CustomProcedure{Name: "P", Owner: "ns.E", SourcePath: "b/y.atl"}
	if !customProcContentEqual(a, b) {
		t.Error("procedures differing only by SourcePath should be content-equal")
	}
	b.Invalidate = "tag:{id}"
	if customProcContentEqual(a, b) {
		t.Error("a real field change should be detected")
	}
}
