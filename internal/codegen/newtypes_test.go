package codegen

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// End to end from source for the types added to the registry, since a type is
// only usable if every layer between the spelling and the DDL carries it:
// parseType's parameter branch, lowering, and the column emitter.
//
// The registry is checked against docs/reference/dsl-types.md elsewhere. This
// checks a declaration a customer could write reaches Postgres-shaped DDL.
func TestNewScalarTypesReachDDL(t *testing.T) {
	src := "entity Legacy in probe {\n" +
		"  id bigint primary\n" +
		"  code char(10)\n" +
		"  label char\n" +
		"  seen_at timestamp\n" +
		"  wallclock time\n" +
		"  zoned timetz\n" +
		"  payload json\n" +
		"  doc xml\n" +
		"  mac macaddr\n" +
		"  mac8 macaddr8\n" +
		"  price money\n" +
		"  ident name\n" +
		"  q tsquery\n" +
		"}\n"

	f, err := dsl.Parse("t.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	scripts, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}

	for _, want := range []string{
		`"code" CHAR(10)`,
		`"label" CHAR`,
		`"seen_at" TIMESTAMP`,
		`"wallclock" TIME`,
		`"zoned" TIMETZ`,
		`"payload" JSON`,
		`"doc" XML`,
		`"mac" MACADDR`,
		`"mac8" MACADDR8`,
		`"price" MONEY`,
		`"ident" NAME`,
		`"q" TSQUERY`,
	} {
		if !strings.Contains(scripts.Up, want) {
			t.Errorf("DDL is missing %s:\n%s", want, scripts.Up)
		}
	}
}

// Postgres cannot sort json, xml or any of the seven geometric types, and
// defines equality on only five of the geometric ones. A filter or an ORDER BY
// on any of them would be DDL-clean and fail at the database, so none carries a
// predicate and none is orderable.
func TestUncomparableTypesCarryNoPredicate(t *testing.T) {
	uncomparable := []string{"json", "xml",
		"point", "line", "lseg", "box", "path", "polygon", "circle"}
	for _, name := range uncomparable {
		ft := dsl.FieldType{Name: name}
		if msg, ok := predicateMessageForField(ft); ok {
			t.Errorf("%s published predicate %q; Postgres cannot execute = on it", name, msg)
		}
		if orderableType(ft) {
			t.Errorf("%s reported orderable; Postgres cannot sort it", name)
		}
	}
	// The types added alongside them that Postgres can compare and sort do
	// carry both.
	filterable := []string{"timestamp", "char", "time", "timetz", "macaddr", "money",
		"name", "tsquery", "inet", "cidr", "bit", "varbit", "tsvector",
		"int4range", "int8range", "numrange", "tsrange", "tstzrange", "daterange"}
	for _, name := range filterable {
		ft := dsl.FieldType{Name: name}
		if _, ok := predicateMessageForField(ft); !ok {
			t.Errorf("%s has no predicate, so it cannot be filtered or serve as a primary key", name)
		}
		if !orderableType(ft) {
			t.Errorf("%s reported not orderable", name)
		}
	}
}

// An array of a text-carried type is refused before any DDL is written.
//
// pgx resolves an array codec from its element's, so with the text codec
// registered `inet[]` scans successfully and returns the binary array body
// reinterpreted as characters. Refusing here keeps a loud failure from
// becoming a silent wrong answer.
func TestArraysOfTextCarriedTypesAreRefused(t *testing.T) {
	src := "entity Hosts in probe {\n" +
		"  id bigint primary\n" +
		"  addrs []inet\n" +
		"}\n"

	f, err := dsl.Parse("t.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if _, err := EmitInitial(ir); err == nil {
		t.Fatal("[]inet was emitted; the column would read back as wire bytes")
	} else if !strings.Contains(err.Error(), "carries") {
		t.Errorf("refused for the wrong reason: %v", err)
	}

	// An array of a binary-carried type is unaffected.
	src = "entity Tags in probe {\n" +
		"  id bigint primary\n" +
		"  labels []text\n" +
		"}\n"
	f, err = dsl.Parse("t.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err = dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if _, err := EmitInitial(ir); err != nil {
		t.Errorf("[]text was refused: %v", err)
	}
}
