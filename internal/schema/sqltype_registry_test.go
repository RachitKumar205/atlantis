package schema_test

import (
	"sort"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/coltype"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// sqlSamples pins one FieldType per registered type, with both the
// parameterised and bare form where a type takes an argument.
var sqlSamples = map[string][]dsl.FieldType{
	"smallint":    {{Name: "smallint"}},
	"int":         {{Name: "int"}},
	"bigint":      {{Name: "bigint"}},
	"real":        {{Name: "real"}},
	"double":      {{Name: "double"}},
	"boolean":     {{Name: "boolean"}},
	"text":        {{Name: "text"}},
	"citext":      {{Name: "citext"}},
	"varchar":     {{Name: "varchar"}, {Name: "varchar", Len: 255}},
	"uuid":        {{Name: "uuid"}},
	"numeric":     {{Name: "numeric"}, {Name: "numeric", NumP: 12, NumS: 2, HasNumP: true}},
	"jsonb":       {{Name: "jsonb"}},
	"bytea":       {{Name: "bytea"}},
	"timestamptz": {{Name: "timestamptz"}},
	"date":        {{Name: "date"}},
	"interval":    {{Name: "interval"}},
	"vector":      {{Name: "vector", VecDim: 3}},
	"timestamp":   {{Name: "timestamp"}},
	"char":        {{Name: "char"}, {Name: "char", Len: 10}},
	"time":        {{Name: "time"}},
	"timetz":      {{Name: "timetz"}},
	"macaddr":     {{Name: "macaddr"}},
	"macaddr8":    {{Name: "macaddr8"}},
	"money":       {{Name: "money"}},
	"tsquery":     {{Name: "tsquery"}},
	"name":        {{Name: "name"}},
	"json":        {{Name: "json"}},
	"xml":         {{Name: "xml"}},
	"bit":         {{Name: "bit"}, {Name: "bit", Len: 8}},
	"varbit":      {{Name: "varbit"}, {Name: "varbit", Len: 8}},
	"inet":        {{Name: "inet"}},
	"cidr":        {{Name: "cidr"}},
	"tsvector":    {{Name: "tsvector"}},
	"int4range":   {{Name: "int4range"}},
	"int8range":   {{Name: "int8range"}},
	"numrange":    {{Name: "numrange"}},
	"tsrange":     {{Name: "tsrange"}},
	"tstzrange":   {{Name: "tstzrange"}},
	"daterange":   {{Name: "daterange"}},
	"point":       {{Name: "point"}},
	"line":        {{Name: "line"}},
	"lseg":        {{Name: "lseg"}},
	"box":         {{Name: "box"}},
	"path":        {{Name: "path"}},
	"polygon":     {{Name: "polygon"}},
	"circle":      {{Name: "circle"}},
}

// Every registered type renders a SQL spelling, in both its bare and array
// forms. A type in the registry with no sample here fails rather than being
// skipped, so a row added without a spelling cannot reach a CREATE TABLE.
//
// The spelling itself is held to the reference page by
// coltype.TestEveryDocumentedTypeIsImplemented; this pins that SQLType and the
// registry stay one thing.
func TestRegistrySQLNameMatchesSQLType(t *testing.T) {
	names := coltype.Names()
	sort.Strings(names)
	for _, n := range names {
		samples, ok := sqlSamples[n]
		if !ok {
			t.Errorf("%s is registered but has no sample here, so nothing checks its SQL spelling", n)
			continue
		}
		for _, ft := range samples {
			got, err := coltype.SQLName(ft)
			if err != nil {
				t.Errorf("%s: SQLName: %v", n, err)
				continue
			}
			if want := schema.SQLType(ft); got != want {
				t.Errorf("%s: registry renders %q, SQLType renders %q", n, got, want)
			}
			arr := dsl.FieldType{Name: n, Array: true, Elem: &ft}
			gotArr, err := coltype.SQLName(arr)
			if err != nil {
				t.Errorf("[]%s: SQLName: %v", n, err)
				continue
			}
			if want := schema.SQLType(arr); gotArr != want {
				t.Errorf("[]%s: registry renders %q, SQLType renders %q", n, gotArr, want)
			}
		}
	}
}

// PredicateKindForField spells the same stem as the registry, prefixed. The
// two must agree before its switch goes.
func TestRegistryStemMatchesPredicateKind(t *testing.T) {
	for _, n := range coltype.Names() {
		ft := dsl.FieldType{Name: n}
		stem, hasStem := coltype.PredicateStem(ft)
		got, ok := schema.PredicateKindForField(ft)
		if hasStem != ok {
			t.Errorf("%s: registry has predicate=%v, PredicateKindForField says %v", n, hasStem, ok)
			continue
		}
		if ok && got != "Predicate"+stem {
			t.Errorf("%s: registry stem %q gives %q, PredicateKindForField gives %q",
				n, stem, "Predicate"+stem, got)
		}
	}
}
