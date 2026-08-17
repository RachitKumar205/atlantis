package query

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	_ "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/common/v1"
)

// Every PredicateKind must reach a translator arm.
//
// translatePredicate ends in a `default` that returns "unsupported predicate
// kind %d" — reachable only through a kind that exists in the enum and nowhere
// in the switch. That is the last of the places a new type can be half-added:
// the emitters agree, the descriptor has the message, the FilterSpec carries
// the kind, and then the request fails at translation time with an error naming
// a number.
//
// The kind list is read out of spec.go rather than restated here. A restated
// list is the failure this whole family of tests exists to prevent, and it
// fails in the worst direction: a kind nobody added to the list is a kind the
// loop never visits, so the test passes by checking less.
func TestEveryPredicateKindHasATranslatorArm(t *testing.T) {
	names := predicateKindNames(t)
	if len(names) < 8 {
		t.Fatalf("parsed %d predicate kinds out of spec.go; the enum has at least "+
			"eight, so the parse is wrong and this test is checking almost nothing",
			len(names))
	}

	for i, name := range names {
		// The iota order in spec.go is the value order; PredicateUnknown is
		// index 0 and legitimately has no arm.
		kind := PredicateUnknown + PredicateKind(i)
		if name == "PredicateUnknown" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			msgName := strings.TrimPrefix(name, "Predicate") + "Predicate"
			mt, err := protoregistry.GlobalTypes.FindMessageByName(
				protoreflect.FullName("atlantis.common.v1." + msgName))
			if err != nil {
				t.Fatalf("kind %s implies message atlantis.common.v1.%s, which is "+
					"not declared: %v", name, msgName, err)
			}

			// An EMPTY predicate message: no oneof arm set. Every translator
			// helper returns ("", nil) for that, so this reaches the dispatch
			// switch and nothing else — the coverage wanted, with no per-kind
			// value fixtures to maintain.
			w := &walker{spec: FilterSpec{EntityID: "t.T"}, nextPH: 1}
			fs := FieldSpec{Column: `"c"`, Kind: kind}
			_, terr := w.translatePredicate(mt.New(), fs)
			if terr != nil && strings.Contains(terr.Error(), "unsupported predicate kind") {
				t.Errorf("translatePredicate has no arm for %s, so a column of that "+
					"kind is emitted as filterable everywhere and then refused at "+
					"request time: %v", name, terr)
			}
		})
	}
}

// A predicate message declared in the proto must be wired to a kind.
//
// The reverse direction. A message added to predicates.proto and forgotten is
// dead weight that reads as supported — a caller browsing the generated client
// finds it and cannot use it.
func TestEveryPredicateMessageHasAKind(t *testing.T) {
	known := map[string]bool{}
	for _, name := range predicateKindNames(t) {
		if name == "PredicateUnknown" {
			continue
		}
		known[strings.TrimPrefix(name, "Predicate")+"Predicate"] = true
	}

	var orphans []string
	protoregistry.GlobalTypes.RangeMessages(func(mt protoreflect.MessageType) bool {
		full := string(mt.Descriptor().FullName())
		if !strings.HasPrefix(full, "atlantis.common.v1.") {
			return true
		}
		short := strings.TrimPrefix(full, "atlantis.common.v1.")
		// List and Range messages are arguments to an arm, not predicates.
		if !strings.HasSuffix(short, "Predicate") {
			return true
		}
		if !known[short] {
			orphans = append(orphans, short)
		}
		return true
	})

	if len(orphans) > 0 {
		t.Errorf("these predicate messages are declared in predicates.proto but no "+
			"PredicateKind maps to them, so no column can ever use one: %s",
			strings.Join(orphans, ", "))
	}
}

var kindConstRe = regexp.MustCompile(`^\s*(Predicate\w+)`)

// predicateKindNames reads the PredicateKind const block out of spec.go, in
// declaration order, so the value of each name is its index.
func predicateKindNames(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("spec.go")
	if err != nil {
		t.Fatalf("read spec.go: %v", err)
	}
	lines := strings.Split(string(src), "\n")

	var out []string
	inBlock := false
	for _, line := range lines {
		if strings.Contains(line, "PredicateUnknown PredicateKind = iota") {
			inBlock = true
			out = append(out, "PredicateUnknown")
			continue
		}
		if !inBlock {
			continue
		}
		if strings.HasPrefix(line, ")") {
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if m := kindConstRe.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}
