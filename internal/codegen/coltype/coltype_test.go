package coltype

import (
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// ft is a tiny constructor so the table rows stay short.
func ft(name string) dsl.FieldType { return dsl.FieldType{Name: name} }

func arrft(elem string) dsl.FieldType {
	e := ft(elem)
	return dsl.FieldType{Array: true, Elem: &e}
}

func TestGoType(t *testing.T) {
	cases := []struct {
		name    string
		t       dsl.FieldType
		notNull bool
		want    string
	}{
		// Not-null scalars.
		{"smallint nn", ft("smallint"), true, "int32"},
		{"int nn", ft("int"), true, "int32"},
		{"bigint nn", ft("bigint"), true, "int64"},
		{"text nn", ft("text"), true, "string"},
		{"varchar nn", ft("varchar"), true, "string"},
		{"citext nn", ft("citext"), true, "string"},
		{"boolean nn", ft("boolean"), true, "bool"},
		{"timestamptz nn", ft("timestamptz"), true, "time.Time"},
		{"date nn", ft("date"), true, "time.Time"},
		{"interval nn", ft("interval"), true, "time.Duration"},
		{"uuid nn", ft("uuid"), true, "string"},
		{"numeric nn", ft("numeric"), true, "string"},

		// Nullable scalars route through pointer.
		{"smallint null", ft("smallint"), false, "*int32"},
		{"int null", ft("int"), false, "*int32"},
		{"bigint null", ft("bigint"), false, "*int64"},
		{"text null", ft("text"), false, "*string"},
		{"varchar null", ft("varchar"), false, "*string"},
		{"boolean null", ft("boolean"), false, "*bool"},
		{"timestamptz null", ft("timestamptz"), false, "*time.Time"},
		{"interval null", ft("interval"), false, "*time.Duration"},
		{"uuid null", ft("uuid"), false, "*string"},
		{"numeric null", ft("numeric"), false, "*string"},

		// Naturally-nullable shapes never get a pointer wrap regardless
		// of the notNull flag — nil zero already encodes absence.
		{"bytea nn", ft("bytea"), true, "[]byte"},
		{"bytea null", ft("bytea"), false, "[]byte"},
		{"jsonb nn", ft("jsonb"), true, "[]byte"},
		{"jsonb null", ft("jsonb"), false, "[]byte"},
		{"vector nn", ft("vector"), true, "[]float32"},
		{"vector null", ft("vector"), false, "[]float32"},

		// Arrays.
		{"[]text", arrft("text"), true, "[]string"},
		{"[]int", arrft("int"), true, "[]int32"},
		{"[]bigint", arrft("bigint"), true, "[]int64"},

		// Unknown types — defensive fallback to `any`.
		{"unknown nn", ft("never_heard_of_it"), true, "any"},
		{"unknown null", ft("never_heard_of_it"), false, "*any"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := GoType(c.t, c.notNull); got != c.want {
				t.Errorf("GoType(%+v, notNull=%v) = %q, want %q", c.t, c.notNull, got, c.want)
			}
		})
	}
}

func TestProtoType(t *testing.T) {
	cases := []struct {
		name string
		t    dsl.FieldType
		want string
	}{
		{"smallint", ft("smallint"), "int32"},
		{"int", ft("int"), "int32"},
		{"bigint", ft("bigint"), "int64"},
		{"text", ft("text"), "string"},
		{"varchar", ft("varchar"), "string"},
		{"citext", ft("citext"), "string"},
		{"boolean", ft("boolean"), "bool"},
		{"timestamptz", ft("timestamptz"), "google.protobuf.Timestamp"},
		{"date", ft("date"), "google.protobuf.Timestamp"},
		{"interval", ft("interval"), "google.protobuf.Duration"},
		{"uuid", ft("uuid"), "string"},
		{"bytea", ft("bytea"), "bytes"},
		{"jsonb", ft("jsonb"), "bytes"},
		{"numeric", ft("numeric"), "string"},
		{"vector", ft("vector"), "repeated float"},
		{"[]text", arrft("text"), "repeated string"},
		{"[]int", arrft("int"), "repeated int32"},
		{"[]bigint", arrft("bigint"), "repeated int64"},
		{"[]vector", arrft("vector"), "repeated repeated float"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ProtoType(c.t)
			if err != nil {
				t.Fatalf("ProtoType(%+v) error: %v", c.t, err)
			}
			if got != c.want {
				t.Errorf("ProtoType(%+v) = %q, want %q", c.t, got, c.want)
			}
		})
	}

	// Unknown types surface an error so codegen fails loudly instead of
	// emitting an unbuildable proto.
	t.Run("unknown returns error", func(t *testing.T) {
		_, err := ProtoType(ft("blorp"))
		if err == nil {
			t.Fatalf("ProtoType(blorp) want error, got nil")
		}
	})
}

func TestScanFragments(t *testing.T) {
	const local = "x"
	const proto = "out.Y"

	cases := []struct {
		name       string
		t          dsl.FieldType
		notNull    bool
		wantDecl   string
		wantTarget string
		wantAssign string
	}{
		// Not-null scalars scan into bare Go types.
		{"int nn", ft("int"), true,
			"var x int32", "&x", "out.Y = x"},
		{"bigint nn", ft("bigint"), true,
			"var x int64", "&x", "out.Y = x"},
		{"text nn", ft("text"), true,
			"var x string", "&x", "out.Y = x"},
		{"varchar nn", ft("varchar"), true,
			"var x string", "&x", "out.Y = x"},
		{"numeric nn", ft("numeric"), true,
			"var x string", "&x", "out.Y = x"},
		{"boolean nn", ft("boolean"), true,
			"var x bool", "&x", "out.Y = x"},
		{"timestamptz nn", ft("timestamptz"), true,
			"var x time.Time", "&x", "out.Y = runtime.TimeToProto(x)"},
		{"date nn", ft("date"), true,
			"var x time.Time", "&x", "out.Y = runtime.TimeToProto(x)"},
		{"interval nn", ft("interval"), true,
			"var x string", "&x", "out.Y = x"},

		// Nullable scalars route through sql.NullX.
		{"int null", ft("int"), false,
			"var x sql.NullInt32", "&x", "out.Y = runtime.Int32PtrFromNull(x)"},
		{"bigint null", ft("bigint"), false,
			"var x sql.NullInt64", "&x", "out.Y = runtime.Int64PtrFromNull(x)"},
		{"text null", ft("text"), false,
			"var x sql.NullString", "&x", "out.Y = runtime.StringPtrFromNull(x)"},
		{"boolean null", ft("boolean"), false,
			"var x sql.NullBool", "&x", "out.Y = runtime.BoolPtrFromNull(x)"},
		{"interval null", ft("interval"), false,
			"var x sql.NullString", "&x", "out.Y = runtime.StringPtrFromNull(x)"},

		// Naturally-nullable shapes ignore the nullability flag.
		{"bytea nn", ft("bytea"), true,
			"var x []byte", "&x", "out.Y = x"},
		{"bytea null", ft("bytea"), false,
			"var x []byte", "&x", "out.Y = x"},
		{"jsonb nn", ft("jsonb"), true,
			"var x []byte", "&x", "out.Y = x"},

		// Vector scans into pgvector.Vector and converts via runtime helper.
		{"vector nn", ft("vector"), true,
			"var x pgvector.Vector", "&x", "out.Y = runtime.VectorToFloat32(x.Slice())"},
		{"vector null", ft("vector"), false,
			"var x *pgvector.Vector", "&x", "if x != nil {\n\t\tout.Y = runtime.VectorToFloat32(x.Slice())\n\t}"},

		// Arrays scan into []elem and assign directly. Elem nullability
		// inside an array is forced not-null per coltype contract.
		{"[]text", arrft("text"), true,
			"var x []string", "&x", "out.Y = x"},
		{"[]int", arrft("int"), true,
			"var x []int32", "&x", "out.Y = x"},
		{"[]bigint", arrft("bigint"), true,
			"var x []int64", "&x", "out.Y = x"},

		// Unknown type → defensive `any` fallback with a marker comment
		// so a codegen bug surfaces visibly in the emitted Go.
		{"unknown nn", ft("blorp"), true,
			"var x any", "&x", "_ = x // unknown type blorp"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			decl, target, assign := ScanFragments(c.t, c.notNull, local, proto)
			if decl != c.wantDecl {
				t.Errorf("decl = %q, want %q", decl, c.wantDecl)
			}
			if target != c.wantTarget {
				t.Errorf("target = %q, want %q", target, c.wantTarget)
			}
			if assign != c.wantAssign {
				t.Errorf("assign = %q, want %q", assign, c.wantAssign)
			}
		})
	}

	// sql.NullTime path emits a multi-line block; pin the exact format
	// here because the assign string lands verbatim in generated Go.
	t.Run("timestamptz null", func(t *testing.T) {
		_, _, assign := ScanFragments(ft("timestamptz"), false, "x", "out.Y")
		const want = `if x.Valid {
		out.Y = runtime.TimeToProto(x.Time)
	}`
		if assign != want {
			t.Errorf("assign =\n%s\nwant\n%s", assign, want)
		}
	})
}

func TestBindExpr(t *testing.T) {
	const getter = "in.GetX()"
	const ptr = "in.X"

	cases := []struct {
		name    string
		t       dsl.FieldType
		notNull bool
		want    string
	}{
		// Not-null scalars pass the getter through.
		{"int nn", ft("int"), true, getter},
		{"bigint nn", ft("bigint"), true, getter},
		{"text nn", ft("text"), true, getter},
		{"numeric nn", ft("numeric"), true, getter},
		{"boolean nn", ft("boolean"), true, getter},
		{"interval nn", ft("interval"), true, getter},

		// Nullable scalars route the pointer through runtime helpers.
		{"int null", ft("int"), false, "runtime.NullableInt32(in.X)"},
		{"bigint null", ft("bigint"), false, "runtime.NullableInt64(in.X)"},
		{"text null", ft("text"), false, "runtime.NullableString(in.X)"},
		{"boolean null", ft("boolean"), false, "runtime.NullableBool(in.X)"},
		{"interval null", ft("interval"), false, "runtime.NullableString(in.X)"},

		// Timestamps use the proto conversion helpers in both directions.
		{"timestamptz nn", ft("timestamptz"), true, "runtime.ProtoToTime(in.GetX())"},
		{"timestamptz null", ft("timestamptz"), false, "runtime.ProtoToTimePtr(in.X)"},
		{"date nn", ft("date"), true, "runtime.ProtoToTime(in.GetX())"},

		// Bytes/JSON pass through unchanged.
		{"bytea", ft("bytea"), true, getter},
		{"jsonb", ft("jsonb"), true, getter},

		// Vectors wrap through pgvector.NewVector.
		{"vector", ft("vector"), true, "pgvector.NewVector(in.GetX())"},

		// Arrays pass the getter through (pgx scans []T directly).
		{"[]text", arrft("text"), true, getter},
		{"[]bigint", arrft("bigint"), true, getter},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BindExpr(c.t, c.notNull, getter, ptr)
			if got != c.want {
				t.Errorf("BindExpr(%+v, notNull=%v) = %q, want %q", c.t, c.notNull, got, c.want)
			}
		})
	}
}

func TestNeedsPgvector(t *testing.T) {
	cases := []struct {
		name string
		t    dsl.FieldType
		want bool
	}{
		{"text", ft("text"), false},
		{"int", ft("int"), false},
		{"bytea", ft("bytea"), false},
		{"jsonb", ft("jsonb"), false},
		{"vector", ft("vector"), true},
		{"[]text", arrft("text"), false},
		{"[]vector", arrft("vector"), true},
		{"array with nil elem", dsl.FieldType{Array: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NeedsPgvector(c.t); got != c.want {
				t.Errorf("NeedsPgvector(%+v) = %v, want %v", c.t, got, c.want)
			}
		})
	}
}

func TestNeedsDatabaseSQL(t *testing.T) {
	cases := []struct {
		name    string
		t       dsl.FieldType
		notNull bool
		want    bool
	}{
		// Not-null path never needs sql.NullX.
		{"text nn", ft("text"), true, false},
		{"int nn", ft("int"), true, false},
		{"vector nn", ft("vector"), true, false},

		// Nullable scalar columns drive the import.
		{"text null", ft("text"), false, true},
		{"int null", ft("int"), false, true},
		{"bigint null", ft("bigint"), false, true},
		{"boolean null", ft("boolean"), false, true},
		{"timestamptz null", ft("timestamptz"), false, true},
		{"date null", ft("date"), false, true},
		{"interval null", ft("interval"), false, true},
		{"uuid null", ft("uuid"), false, true},
		{"numeric null", ft("numeric"), false, true},

		// Shapes whose nullable path doesn't use sql.NullX.
		{"bytea null", ft("bytea"), false, false},
		{"jsonb null", ft("jsonb"), false, false},
		{"vector null", ft("vector"), false, false},
		{"[]text", arrft("text"), false, false},

		// Unknown types: defensive false so a stray "blorp" doesn't drag
		// in database/sql; the codegen surfaces via the `var x any` path.
		{"unknown null", ft("blorp"), false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NeedsDatabaseSQL(c.t, c.notNull); got != c.want {
				t.Errorf("NeedsDatabaseSQL(%+v, notNull=%v) = %v, want %v", c.t, c.notNull, got, c.want)
			}
		})
	}
}
