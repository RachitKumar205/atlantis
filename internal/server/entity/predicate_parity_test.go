package entity

import (
	"strings"
	"testing"

	_ "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/common/v1"

	"github.com/rachitkumar205/atlantis/internal/codegen/query"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
	"github.com/rachitkumar205/atlantis/internal/testsupport/dsltypes"
)

// The dispatcher's half of the predicate parity check.
//
// Its predicateMessageForField carries the comment "Mirrors
// codegen/query_emit.go predicateMessageForField" — a mirror that nothing
// checked. Both are asserted against schema.PredicateKindForField rather than
// against each other, so the shared authority is what they agree on and a
// third implementation has somewhere obvious to anchor.
//
// See internal/codegen/predicate_parity_test.go for the full account of what
// drifted and why the kind↔message naming rule is asserted rather than listed.
func TestDispatcherPredicateTableAgrees(t *testing.T) {
	rows, err := dsltypes.Rows()
	if err != nil {
		t.Fatalf("read documented types: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no documented types parsed; every assertion below would be vacuous")
	}

	checked := 0
	for _, r := range rows {
		if r.ATL == dsltypes.ArrayRowSpelling {
			continue
		}
		t.Run(r.ATL, func(t *testing.T) {
			ft := dsl.FieldType{Name: r.ATL}
			if i := strings.IndexByte(r.ATL, '('); i > 0 {
				ft = dsl.FieldType{Name: strings.TrimSpace(r.ATL[:i])}
			}

			kind, hasKind := schema.PredicateKindForField(ft)
			msg, hasMsg := predicateMessageForField(ft)

			if hasKind != hasMsg {
				t.Fatalf("filterability disagrees: schema says %v, the dispatcher's "+
					"predicateMessageForField says %v. The dispatcher publishes the "+
					"filter message a caller sends; a type present in one table and "+
					"absent from the other is either an unreachable field or a field "+
					"the translator will refuse", hasKind, hasMsg)
			}
			if !hasKind {
				return
			}
			checked++

			if want := strings.TrimPrefix(kind, "Predicate") + "Predicate"; msg != want {
				t.Errorf("kind %q pairs with message %q, but the dispatcher returns %q",
					kind, want, msg)
			}

			// The round-trip that actually runs at request time: the emitted
			// FilterSpec carries the kind as a STRING, and the dispatcher turns
			// it back into a query.PredicateKind. A name this function does not
			// know becomes PredicateUnknown and the filter is rejected.
			if got := predicateKindFromString(kind); got == query.PredicateUnknown {
				t.Errorf("predicateKindFromString(%q) = PredicateUnknown, so a "+
					"FilterSpec naming this kind is refused at request time even "+
					"though every emitter agrees the column is filterable", kind)
			}
		})
	}

	if checked < 6 {
		t.Errorf("only %d documented types were checked as filterable; the table "+
			"has drifted far enough that this is no longer covering the path", checked)
	}
}
