package coltype_test

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/coltype"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
	"github.com/rachitkumar205/atlantis/internal/testsupport/dsltypes"
)

// These pin four of the six type-mapping tables against the reference page.
// The reasoning for reading the page rather than restating it lives in the
// dsltypes package doc; the sixth table, the runtime dispatcher's, is pinned
// in internal/server/entity because its emitter is unexported.
//
// A type documented with no sample below FAILS rather than being skipped —
// that is the point. `real` and `double` were documented and implemented
// nowhere, and adding a row to the page without teaching the mappings about it
// is the bug this catches.

// typeSample pins one concrete value per documented spelling.
//
// goOverride exists because the page's Go column describes the type the
// CALLER sees on the generated message, while coltype.GoType names the
// scan-side Go type. Those coincide for scalars and diverge for the three
// shapes that travel as protobuf well-known types.
type typeSample struct {
	ft dsl.FieldType
	// placeholders maps the letters the page uses in a parameterised
	// spelling to the numbers this sample chose.
	placeholders map[string]string
	goOverride   string
}

var samples = map[string]typeSample{
	"bigint":        {ft: dsl.FieldType{Name: "bigint"}},
	"int":           {ft: dsl.FieldType{Name: "int"}, goOverride: "int32"},
	"smallint":      {ft: dsl.FieldType{Name: "smallint"}},
	"real":          {ft: dsl.FieldType{Name: "real"}},
	"double":        {ft: dsl.FieldType{Name: "double"}},
	"boolean":       {ft: dsl.FieldType{Name: "boolean"}},
	"text":          {ft: dsl.FieldType{Name: "text"}},
	"citext":        {ft: dsl.FieldType{Name: "citext"}},
	"jsonb":         {ft: dsl.FieldType{Name: "jsonb"}},
	"bytea":         {ft: dsl.FieldType{Name: "bytea"}},
	"uuid":          {ft: dsl.FieldType{Name: "uuid"}},
	"varchar":       {ft: dsl.FieldType{Name: "varchar"}},
	"varchar(N)":    {ft: dsl.FieldType{Name: "varchar", Len: 255}, placeholders: map[string]string{"N": "255"}},
	"numeric(p, s)": {ft: dsl.FieldType{Name: "numeric", NumP: 12, NumS: 2, HasNumP: true}, placeholders: map[string]string{"p": "12", "s": "2"}},

	// The page names the wire-side Go type for these; GoType names the type
	// the row scans into on the way out of Postgres. Both are correct for
	// their side, so the override records the scan-side answer rather than
	// letting the assertion be skipped.
	"timestamptz": {ft: dsl.FieldType{Name: "timestamptz"}, goOverride: "time.Time"},
	"date":        {ft: dsl.FieldType{Name: "date"}, goOverride: "time.Time"},
	"interval":    {ft: dsl.FieldType{Name: "interval"}, goOverride: "pgtype.Interval"},
	"vector(N)":   {ft: dsl.FieldType{Name: "vector", VecDim: 3}, placeholders: map[string]string{"N": "3"}, goOverride: "[]float32"},

	"char(N)":  {ft: dsl.FieldType{Name: "char", Len: 10}, placeholders: map[string]string{"N": "10"}},
	"char":     {ft: dsl.FieldType{Name: "char"}},
	"name":     {ft: dsl.FieldType{Name: "name"}},
	"money":    {ft: dsl.FieldType{Name: "money"}},
	"xml":      {ft: dsl.FieldType{Name: "xml"}},
	"tsquery":  {ft: dsl.FieldType{Name: "tsquery"}},
	"json":     {ft: dsl.FieldType{Name: "json"}},
	"time":     {ft: dsl.FieldType{Name: "time"}},
	"timetz":   {ft: dsl.FieldType{Name: "timetz"}},
	"macaddr":  {ft: dsl.FieldType{Name: "macaddr"}},
	"macaddr8": {ft: dsl.FieldType{Name: "macaddr8"}},

	// The page names the wire-side type; GoType names the scan side.
	"timestamp": {ft: dsl.FieldType{Name: "timestamp"}, goOverride: "time.Time"},

	"bit(N)":    {ft: dsl.FieldType{Name: "bit", Len: 8}, placeholders: map[string]string{"N": "8"}},
	"bit":       {ft: dsl.FieldType{Name: "bit"}},
	"varbit(N)": {ft: dsl.FieldType{Name: "varbit", Len: 8}, placeholders: map[string]string{"N": "8"}},
	"varbit":    {ft: dsl.FieldType{Name: "varbit"}},
	"inet":      {ft: dsl.FieldType{Name: "inet"}},
	"cidr":      {ft: dsl.FieldType{Name: "cidr"}},
	"tsvector":  {ft: dsl.FieldType{Name: "tsvector"}},
	"int4range": {ft: dsl.FieldType{Name: "int4range"}},
	"int8range": {ft: dsl.FieldType{Name: "int8range"}},
	"numrange":  {ft: dsl.FieldType{Name: "numrange"}},
	"tsrange":   {ft: dsl.FieldType{Name: "tsrange"}},
	"tstzrange": {ft: dsl.FieldType{Name: "tstzrange"}},
	"daterange": {ft: dsl.FieldType{Name: "daterange"}},
	"point":     {ft: dsl.FieldType{Name: "point"}},
	"line":      {ft: dsl.FieldType{Name: "line"}},
	"lseg":      {ft: dsl.FieldType{Name: "lseg"}},
	"box":       {ft: dsl.FieldType{Name: "box"}},
	"path":      {ft: dsl.FieldType{Name: "path"}},
	"polygon":   {ft: dsl.FieldType{Name: "polygon"}},
	"circle":    {ft: dsl.FieldType{Name: "circle"}},
}

func TestEveryDocumentedTypeIsImplemented(t *testing.T) {
	rows, err := dsltypes.Rows()
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.ATL] = true
		if r.ATL == dsltypes.ArrayRowSpelling {
			continue
		}
		t.Run(r.ATL, func(t *testing.T) {
			s, ok := samples[r.ATL]
			if !ok {
				t.Fatalf("%s is documented but has no sample here, so nothing "+
					"checks that the mappings implement it", r.ATL)
			}

			wantSQL := dsltypes.Substitute(r.PG, s.placeholders)
			if got := schema.SQLType(s.ft); !dsltypes.SQLTypeEqual(got, wantSQL) {
				t.Errorf("SQLType = %q, the reference page promises %q", got, wantSQL)
			}

			wantProto := dsltypes.Substitute(r.Proto, s.placeholders)
			gotProto, err := coltype.ProtoType(s.ft)
			if err != nil {
				t.Fatalf("ProtoType: %v — a documented type that codegen cannot "+
					"emit fails at `tide codegen`, after the DDL already ran", err)
			}
			if gotProto != wantProto {
				t.Errorf("ProtoType = %q, the reference page promises %q", gotProto, wantProto)
			}

			// Asserting the exact Go type, not merely that it is not "any".
			// The weaker check passed with `real` mapped to float64 or even
			// string, and GoType is what types the JSON-tagged structs in the
			// customer's generated handlers and the memcached payload — a
			// wrong answer there is silent.
			wantGo := s.goOverride
			if wantGo == "" {
				wantGo = dsltypes.Substitute(r.Go, s.placeholders)
			}
			if got := coltype.GoType(s.ft, true); got != wantGo {
				t.Errorf("GoType = %q, want %q", got, wantGo)
			}
			// The nullable form of a scalar is the pointer form. Shapes whose
			// zero value already encodes absence keep their bare type.
			gotNullable := coltype.GoType(s.ft, false)
			wantNullable := "*" + wantGo
			switch wantGo {
			// pgtype.Interval joins these: it carries its own Valid flag, so
			// the nullable form is the same type rather than a pointer to it.
			case "[]byte", "[]float32", "pgtype.Interval":
				wantNullable = wantGo
			}
			if gotNullable != wantNullable {
				t.Errorf("GoType(nullable) = %q, want %q", gotNullable, wantNullable)
			}
		})
	}

	// The reverse direction: a sample naming a type the page does not list is
	// either a typo here or an undocumented type, and both make the loop above
	// quieter than it looks.
	for name := range samples {
		if !seen[name] {
			t.Errorf("sample %q matches no row in %s", name, dsltypes.DocPath)
		}
	}
}

// TestDocumentedArrayMapping covers the row the loop skips. The page states
// that mapping as a template; this pins it on a concrete element type.
func TestDocumentedArrayMapping(t *testing.T) {
	ft := dsl.FieldType{Name: "text", Array: true, Elem: &dsl.FieldType{Name: "text"}}
	if got := schema.SQLType(ft); got != "TEXT[]" {
		t.Errorf("SQLType = %q, want TEXT[]", got)
	}
	got, err := coltype.ProtoType(ft)
	if err != nil {
		t.Fatalf("ProtoType: %v", err)
	}
	if got != "repeated string" {
		t.Errorf("ProtoType = %q, want repeated string", got)
	}
	if got := coltype.GoType(ft, true); got != "[]string" {
		t.Errorf("GoType = %q, want []string", got)
	}
}

// TestScanAndBindKnowEveryDocumentedType covers the two mappings the loop
// cannot check against the page, because the page describes neither scan
// locals nor bind expressions. Both have a silent fallback, and both fallbacks
// lose data rather than failing.
func TestScanAndBindKnowEveryDocumentedType(t *testing.T) {
	const bareGetter = "req.GetF()"
	for name, s := range samples {
		for _, notNull := range []bool{true, false} {
			decl, target, assign := coltype.ScanFragments(s.ft, notNull, "v", "out.F")
			if strings.Contains(assign, "unknown type") {
				t.Errorf("%s (notNull=%v): ScanFragments fell through to the "+
					"unknown-type branch, so a read scans into `any` and drops "+
					"the value: %s / %s", name, notNull, decl, assign)
			}
			// A scan fragment that declares a local and never copies it into
			// the proto field reads the column and discards it just as surely
			// as the unknown-type branch does.
			if decl == "" || target == "" || assign == "" {
				t.Errorf("%s (notNull=%v): ScanFragments returned an empty piece "+
					"(decl=%q target=%q assign=%q)", name, notNull, decl, target, assign)
			}

			// BindExpr has no error branch: an unhandled type returns the bare
			// getter. That is CORRECT for the types whose Go form is already
			// nilable — []byte, []float32, []T all carry null as nil — and
			// wrong for a nullable scalar, where the getter yields the zero
			// value and the write silently stores 0 or "" instead of NULL.
			//
			// GoType's nullable form separates the two: a leading `*` means
			// the scalar needs an explicit null-carrying wrapper. Which
			// wrapper is not fixed — nullable scalars go through
			// runtime.NullableX, timestamps through runtime.ProtoToTimePtr —
			// so this asserts that SOME wrapping happens rather than naming
			// one, which would fail the next type that needs a third
			// mechanism for a good reason.
			if notNull || !strings.HasPrefix(coltype.GoType(s.ft, false), "*") {
				continue
			}
			if bind := coltype.BindExpr(s.ft, false, bareGetter, "req.F"); bind == bareGetter {
				t.Errorf("%s: nullable, but BindExpr returns the bare getter, so "+
					"an absent value writes the zero value instead of NULL", name)
			}
		}
	}
}
