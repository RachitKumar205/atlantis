package codegen

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

const enumSchema = "enum Mood in app { happy, sad, \"in progress\" }\n" +
	"\n" +
	"entity Person in app {\n" +
	"  id bigint primary\n" +
	"  mood Mood\n" +
	"  fallback Mood not null\n" +
	"}\n"

// End to end from source, since every layer between the keyword and the DDL is
// new: token, AST node, top-level dispatch, lowering, field resolution and the
// emitter.
func TestEnumReachesDDL(t *testing.T) {
	f, err := dsl.Parse("t.atl", []byte(enumSchema))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if len(ir.Enums) != 1 {
		t.Fatalf("lowered %d enums, want 1", len(ir.Enums))
	}
	if got, want := ir.Enums[0].ID(), "app.Mood"; got != want {
		t.Errorf("enum ID = %q, want %q", got, want)
	}
	if got, want := strings.Join(ir.Enums[0].Values, "|"), "happy|sad|in progress"; got != want {
		t.Errorf("labels = %q, want %q", got, want)
	}

	// The field resolved against the declaration rather than staying an
	// unknown scalar named "Mood".
	mood := ir.Entities[0].FindField("mood")
	if mood == nil {
		t.Fatal("no mood field")
	}
	if !mood.Type.Enum {
		t.Errorf("mood is not marked as an enum: %+v", mood.Type)
	}
	if got, want := mood.Type.Name, "app.Mood"; got != want {
		t.Errorf("mood type = %q, want the enum ID %q", got, want)
	}

	scripts, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	wantType := `CREATE TYPE "atlantis"."app_mood" AS ENUM ('happy', 'sad', 'in progress');`
	if !strings.Contains(scripts.Up, wantType) {
		t.Errorf("DDL is missing %s:\n%s", wantType, scripts.Up)
	}
	if !strings.Contains(scripts.Up, `"mood" "atlantis"."app_mood"`) {
		t.Errorf("the column does not name the enum type:\n%s", scripts.Up)
	}

	// Ordering is the property that matters: Postgres refuses a column whose
	// type does not exist yet.
	if strings.Index(scripts.Up, wantType) > strings.Index(scripts.Up, `"mood"`) {
		t.Errorf("CREATE TYPE comes after the table that uses it:\n%s", scripts.Up)
	}
}

// An enum column is a string on the wire, and Postgres constrains it. There is
// no proto enum yet, so the label is what a caller sends and receives.
func TestEnumColumnIsAString(t *testing.T) {
	f, err := dsl.Parse("t.atl", []byte(enumSchema))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	mood := ir.Entities[0].FindField("mood")

	if msg, ok := predicateMessageForField(mood.Type); !ok || msg != "StringPredicate" {
		t.Errorf("predicate = %q (ok=%v), want StringPredicate — an enum compares "+
			"and sorts in Postgres, so it is filterable", msg, ok)
	}
	if !orderableType(mood.Type) {
		t.Error("an enum reported not orderable; Postgres sorts one by label order")
	}
}

// Postgres has no empty enum type, and no way to hold a label twice.
func TestEnumRefusesShapesPostgresWould(t *testing.T) {
	for name, src := range map[string]string{
		"no labels":        "enum Empty in app { }\n",
		"duplicate label":  "enum Dup in app { a, b, a }\n",
		"array of an enum": "enum Mood in app { happy }\nentity P in app {\n  id bigint primary\n  moods []Mood\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			f, err := dsl.Parse("t.atl", []byte(src))
			if err != nil {
				return // a parse error is also a refusal
			}
			if _, err := dsl.Lower([]*dsl.File{f}); err == nil {
				t.Error("lowered without error")
			}
		})
	}
}

// The diff sees enum changes.
//
// A differ that does not read them reports "0 changes" for a schema whose enum
// gained a label, and the apply then runs no DDL for it.
func TestEnumChangesAreDiffed(t *testing.T) {
	lower := func(t *testing.T, src string) *dsl.IR {
		t.Helper()
		f, err := dsl.Parse("t.atl", []byte(src))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		ir, err := dsl.Lower([]*dsl.File{f})
		if err != nil {
			t.Fatalf("lower: %v", err)
		}
		return ir
	}
	entity := "entity Person in app {\n  id bigint primary\n  mood Mood not null\n}\n"

	base := lower(t, "enum Mood in app { happy, sad }\n"+entity)
	added := lower(t, "enum Mood in app { happy, sad, neutral }\n"+entity)
	removed := lower(t, "enum Mood in app { happy }\n"+entity)
	gone := lower(t, "entity Person in app {\n  id bigint primary\n}\n")

	find := func(d *Diff, k ChangeKind) *Change {
		for _, c := range d.All() {
			if c.Kind == k {
				return &c
			}
		}
		return nil
	}

	t.Run("a label added is additive", func(t *testing.T) {
		c := find(ComputeDiff(base, added), KindEnumValueAdded)
		if c == nil {
			t.Fatal("adding a label produced no change")
		}
		if c.Class != ClassAdditive {
			t.Errorf("class = %v, want additive", c.Class)
		}
	})

	t.Run("a label removed is destructive", func(t *testing.T) {
		c := find(ComputeDiff(base, removed), KindEnumValueRemoved)
		if c == nil {
			t.Fatal("removing a label produced no change")
		}
		if c.Class != ClassDestructive {
			t.Errorf("class = %v, want destructive — Postgres has no ALTER TYPE DROP VALUE", c.Class)
		}
	})

	t.Run("a type removed is destructive", func(t *testing.T) {
		c := find(ComputeDiff(base, gone), KindEnumRemoved)
		if c == nil {
			t.Fatal("removing the type produced no change")
		}
		if c.Class != ClassDestructive {
			t.Errorf("class = %v, want destructive", c.Class)
		}
	})

	t.Run("a type added is additive", func(t *testing.T) {
		c := find(ComputeDiff(gone, base), KindEnumAdded)
		if c == nil {
			t.Fatal("adding the type produced no change")
		}
		if c.Class != ClassAdditive {
			t.Errorf("class = %v, want additive", c.Class)
		}
	})

	t.Run("no change is no diff", func(t *testing.T) {
		same := lower(t, "enum Mood in app { happy, sad }\n"+entity)
		if d := ComputeDiff(base, same); !d.IsEmpty() {
			t.Errorf("an unchanged schema diffed: %v", d.All())
		}
	})
}

// A column changing from a scalar to an enum is a type change, not a silent
// no-op. typeEqual compares the enum marker as well as the name.
func TestAScalarBecomingAnEnumIsAChange(t *testing.T) {
	scalar := dsl.FieldType{Name: "app.Mood"}
	enum := dsl.FieldType{Name: "app.Mood", Enum: true}
	if typeEqual(scalar, enum) {
		t.Error("a scalar and an enum of the same name compared equal, so the change emits no DDL")
	}
}
