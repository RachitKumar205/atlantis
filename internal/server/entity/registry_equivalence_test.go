package entity

import (
	"testing"

	"github.com/rachitkumar205/atlantis/internal/coltype"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// The runtime dispatcher carries its own copy of the predicate mapping. It is
// replaced by a registry lookup, so it must already agree with the registry.
func TestRegistryMatchesRuntimePredicate(t *testing.T) {
	for _, n := range coltype.Names() {
		ft := dsl.FieldType{Name: n}
		stem, hasStem := coltype.PredicateStem(ft)
		msg, ok := predicateMessageForField(ft)
		if hasStem != ok {
			t.Errorf("%s: registry has predicate=%v, predicateMessageForField says %v", n, hasStem, ok)
			continue
		}
		if ok && msg != stem+"Predicate" {
			t.Errorf("%s: registry stem %q gives %q, predicateMessageForField gives %q",
				n, stem, stem+"Predicate", msg)
		}
	}
}
