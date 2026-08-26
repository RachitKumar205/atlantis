package codegen

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	_ "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/common/v1"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
	"github.com/rachitkumar205/atlantis/internal/testsupport/dsltypes"
)

// A type is filterable only if several independent tables agree, and they are
// spread across three packages.
//
// A type added to every type table — DDL, Go type, proto type, scan and bind —
// and to none of the predicate tables can be ordered by and not filtered on.
// Nothing fails; the field is absent from the generated filter message.
//
// The same shape sits one level down. internal/codegen holds its own copy of
// schema.PredicateKindForField, so teaching the two predicateMessageForField
// tables about a type without it produces a .proto carrying the predicate field
// and a FilterSpec with no entry for the column: the filter is advertised, then
// rejected as an unknown field at request time.
//
// The kind name and the message name are the same fact spelled two ways:
// `PredicateFloat` ↔ `FloatPredicate`. Asserting the correspondence turns "do
// the tables agree" into arithmetic instead of a hand-maintained pair list that
// would need updating for every new type — which is the maintenance burden that
// produced the drift in the first place.
//
// The dispatcher's half of this lives in
// internal/server/entity/predicate_parity_test.go; the translator's in
// internal/codegen/query. All three anchor on schema.PredicateKindForField so
// they agree with each other by construction rather than by inspection.
func TestPredicateTablesAgree(t *testing.T) {
	rows, err := dsltypes.Rows()
	if err != nil {
		t.Fatalf("read documented types: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no documented types parsed; every assertion below would be vacuous")
	}

	filterable := 0
	for _, r := range rows {
		if r.ATL == dsltypes.ArrayRowSpelling {
			continue
		}
		t.Run(r.ATL, func(t *testing.T) {
			ft := dsl.FieldType{Name: r.ATL}
			// varchar(N) and numeric(p,s) spell a base type with parameters;
			// the predicate tables switch on the base name only.
			if i := strings.IndexByte(r.ATL, '('); i > 0 {
				ft = dsl.FieldType{Name: strings.TrimSpace(r.ATL[:i])}
			}

			kind, hasKind := schema.PredicateKindForField(ft)
			msg, hasMsg := predicateMessageForField(ft)

			if hasKind != hasMsg {
				t.Fatalf("filterability disagrees: schema.PredicateKindForField says %v, "+
					"codegen.predicateMessageForField says %v. One of them decides "+
					"whether the emitted FilterSpec has an entry and the other whether "+
					"the emitted .proto has a field; a type with only one is advertised "+
					"as filterable and refused at request time", hasKind, hasMsg)
			}
			if !hasKind {
				return
			}
			filterable++

			if want := messageNameForKind(kind); msg != want {
				t.Errorf("kind %q pairs with message %q, but the table returns %q. "+
					"The two names are the same fact spelled twice and must track",
					kind, want, msg)
			}

			// The message has to exist in the compiled proto, or codegen emits a
			// .proto that protoc cannot compile in the caller's repo — where it
			// is far more expensive to discover.
			full := "atlantis.common.v1." + msg
			if _, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(full)); err != nil {
				t.Errorf("%s names predicate message %s, which is not declared in "+
					"atlantis/common/v1/predicates.proto: %v", r.ATL, full, err)
			}
		})
	}

	// Guard against a fixture that filters nothing: if the documented table
	// ever stopped yielding filterable types, every check above would pass by
	// skipping.
	if filterable < 6 {
		t.Errorf("only %d documented types are filterable; the table has drifted "+
			"far enough that this test is no longer covering the predicate path",
			filterable)
	}
}

// messageNameForKind derives `FloatPredicate` from `PredicateFloat`. The
// correspondence holds for all nine kinds; a new kind that breaks it should
// break this test rather than quietly wire a column to the wrong predicate.
func messageNameForKind(kind string) string {
	return strings.TrimPrefix(kind, "Predicate") + "Predicate"
}
