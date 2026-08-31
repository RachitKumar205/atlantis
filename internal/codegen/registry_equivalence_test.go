package codegen

import (
	"testing"

	"github.com/rachitkumar205/atlantis/internal/coltype"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// predicateMessageForField and orderableType are replaced by registry
// lookups, so they must already agree with the registry on every type it
// carries.
func TestRegistryMatchesPredicateAndOrderable(t *testing.T) {
	for _, n := range coltype.Names() {
		ft := dsl.FieldType{Name: n}

		stem, hasStem := coltype.PredicateStem(ft)
		msg, ok := predicateMessageForField(ft)
		if hasStem != ok {
			t.Errorf("%s: registry has predicate=%v, predicateMessageForField says %v", n, hasStem, ok)
		} else if ok && msg != stem+"Predicate" {
			t.Errorf("%s: registry stem %q gives %q, predicateMessageForField gives %q",
				n, stem, stem+"Predicate", msg)
		}

		if got, want := coltype.Orderable(ft), orderableType(ft); got != want {
			t.Errorf("%s: registry orderable=%v, orderableType=%v", n, got, want)
		}

		arr := dsl.FieldType{Name: n, Array: true, Elem: &ft}
		if got, want := coltype.Orderable(arr), orderableType(arr); got != want {
			t.Errorf("[]%s: registry orderable=%v, orderableType=%v", n, got, want)
		}
	}
}
