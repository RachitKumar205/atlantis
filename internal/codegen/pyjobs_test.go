package codegen

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/rachitkumar205/atlantis/internal/coltype"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// TestPyJSONTypeMatchesGoJSONProjection runs encoding/json over a value of
// each class's Go type and checks that the annotation is the one json.loads
// justifies.
//
// Neither side is written down: the sample is checked against GoType, marshalled,
// and the resulting JSON kind compared to PyJSONType's answer. A table naming
// the annotation as well would restate the mapping and agree with it however
// it changed.
//
// The projection is the whole content of PyJSONType, and reading it off
// GoType is what produced the two errors this pins: time.Time is a string,
// not a number, and pgtype.Interval is an object with capitalised keys, not
// a duration.
func TestPyJSONTypeMatchesGoJSONProjection(t *testing.T) {
	// One sample per class, plus the aliases whose class is easy to mistake.
	samples := map[string]any{
		"bigint":      int64(2),
		"int":         int32(2),
		"double":      1.5,
		"real":        float32(1.5),
		"text":        "s",
		"uuid":        "s",
		"numeric":     "1.25",
		"boolean":     true,
		"timestamptz": time.Unix(0, 0).UTC(),
		"date":        time.Unix(0, 0).UTC(),
		"bytea":       []byte("ab"),
		"jsonb":       []byte(`{"a":1}`),
		"interval":    pgtype.Interval{Months: 1, Valid: true},
		"vector":      []float32{0.5},
	}

	for _, atl := range sortedSampleNames(samples) {
		t.Run(atl, func(t *testing.T) {
			ft := dsl.FieldType{Name: atl}
			value := samples[atl]

			// The sample has to be a value of the type the tree says the
			// column is, or the marshal below measures something else.
			if got, want := goTypeName(value), coltype.GoType(ft, true); got != want {
				t.Fatalf("sample is a %s but GoType(%s) is %s", got, atl, want)
			}

			annotation, err := coltype.PyJSONType(ft, true)
			if err != nil {
				t.Fatalf("PyJSONType: %v", err)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			kind := kindOf(string(raw))
			if !annotationFits(annotation, kind) {
				t.Errorf("encoding/json wrote %s as %s, which json.loads reads as "+
					"a %s, but PyJSONType annotates it %q",
					atl, raw, kind.python(), annotation)
			}
		})
	}
}

func sortedSampleNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// goTypeName spells a value's type the way coltype does. reflect prints a
// byte slice as []uint8, which is the same type under another name.
func goTypeName(v any) string {
	name := reflect.TypeOf(v).String()
	if name == "[]uint8" {
		return "[]byte"
	}
	return name
}

// annotationFits reports whether a Python annotation admits the type
// json.loads produces for this JSON kind.
func annotationFits(annotation string, k jsonKind) bool {
	switch {
	case strings.HasPrefix(annotation, "Sequence["):
		return k == jsonArray
	case annotation == "IntervalJSON":
		return k == jsonObject
	case annotation == "str":
		return k == jsonString
	case annotation == "bool":
		return k == jsonBool
	case annotation == "int":
		return k == jsonInt
	case annotation == "float":
		// PEP 484's numeric tower: an int is accepted wherever a float is
		// annotated, and an integral double marshals with no decimal point.
		return k == jsonFloat || k == jsonInt
	}
	return false
}

// TestANullableArgIsOptionalInPython pins the difference from PyType: a
// nullable arg is *T in Go and marshals to null, which the annotation has to
// carry because JSON has no presence bit.
func TestANullableArgIsOptionalInPython(t *testing.T) {
	var absent *int64
	raw, err := json.Marshal(absent)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "null" {
		t.Fatalf("a nil *int64 marshals to %s, not null", raw)
	}
	got, err := coltype.PyJSONType(dsl.FieldType{Name: "bigint"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "int | None" {
		t.Errorf("PyJSONType(nullable) = %q, want %q", got, "int | None")
	}
}

// TestEveryTypeAJobArgCanDeclareHasAPythonAnnotation walks the same registry
// lowering accepts args from. A type that reaches an args block without a
// mapping fails `tide generate` for the caller, after their .atl already
// parsed.
func TestEveryTypeAJobArgCanDeclareHasAPythonAnnotation(t *testing.T) {
	for _, name := range coltype.Names() {
		if _, err := coltype.PyJSONType(dsl.FieldType{Name: name}, true); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestPyJobsEmitsNothingForAnIRWithNoJobs(t *testing.T) {
	files, err := EmitPyJobsHandlers(&dsl.IR{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("emitted %d files for an IR declaring no job", len(files))
	}
}

// TestPyJobsReadsNotNullArgsWithoutADefault covers the branch that decides
// between [] and .get(). Reading a not-null arg with .get() hands the handler
// None for a value the schema says cannot be absent, and the annotation says
// it is not optional — so nothing downstream reports it.
func TestPyJobsReadsNotNullArgsWithoutADefault(t *testing.T) {
	ir := &dsl.IR{Jobs: []dsl.Job{{
		Namespace: "library",
		Name:      "Reindex",
		Args: []dsl.Field{
			{Name: "author_id", Type: dsl.FieldType{Name: "bigint"}, NotNull: true},
			{Name: "reason", Type: dsl.FieldType{Name: "text"}},
		},
	}}}
	files, err := EmitPyJobsHandlers(ir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("emitted %d files, want 1", len(files))
	}
	src := files[0].Content
	for _, want := range []string{
		`author_id=args["author_id"],`,
		`reason=args.get("reason"),`,
		"author_id: int\n",
		"reason: str | None\n",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("emitted jobs.py does not contain %q:\n%s", want, src)
		}
	}
}

type jsonKind int

const (
	jsonInt jsonKind = iota
	jsonFloat
	jsonString
	jsonBool
	jsonObject
	jsonArray
	jsonNull
)

func (k jsonKind) String() string {
	return [...]string{"int", "float", "string", "bool", "object", "array", "null"}[k]
}

// python names the type json.loads yields for this kind.
func (k jsonKind) python() string {
	return [...]string{"int", "float", "str", "bool", "dict", "list", "None"}[k]
}

func kindOf(raw string) jsonKind {
	switch {
	case strings.HasPrefix(raw, `"`):
		return jsonString
	case strings.HasPrefix(raw, "{"):
		return jsonObject
	case strings.HasPrefix(raw, "["):
		return jsonArray
	case raw == "true" || raw == "false":
		return jsonBool
	case raw == "null":
		return jsonNull
	case strings.ContainsAny(raw, ".eE"):
		return jsonFloat
	}
	// An integral float marshals without a point and lands here, so a `double`
	// holding 2.0 reaches Python as an int. PEP 484's numeric tower accepts an
	// int wherever a float is annotated, so the annotation stays "float".
	return jsonInt
}
