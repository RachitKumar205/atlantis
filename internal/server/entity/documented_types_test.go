package entity

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	_ "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/common/v1"
	"github.com/rachitkumar205/atlantis/internal/codegen/coltype"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/testsupport/dsltypes"
)

// The dispatcher publishes its own protobuf descriptors, so it carries a
// SECOND copy of the DSL-type-to-wire-type mapping. dslFieldToProtoField's
// comment claims it uses "the same type mapping as coltype.ProtoType", and
// nothing checked that.
//
// It was false. `real` and `double` were absent from setProtoType, so they hit
// the default arm and were published as proto `string` while `tide codegen`
// published them as `float` and `double` for the same schema. A generated
// client putting fixed32 on the wire against a descriptor that says string
// unmarshals without error, leaves the field unset, and the write reaches
// Postgres as "" — `invalid input syntax for type double precision`. The read
// path lost the value silently: makeScanTarget had no arm either, so the row
// was stringified through fmt.Sprintf into a field the client decodes as 0.
//
// That gap was unreachable until the codegen tables learned the two types,
// because apply failed at the DDL first. Making one half work is what exposed
// the other, which is the argument for pinning them to each other rather than
// each to the page separately.

// protoKindForATL maps what coltype.ProtoType returns to the descriptor kind
// the dispatcher must produce for the same column.
var protoKindForATL = map[string]descriptorpb.FieldDescriptorProto_Type{
	"int32":                     descriptorpb.FieldDescriptorProto_TYPE_INT32,
	"int64":                     descriptorpb.FieldDescriptorProto_TYPE_INT64,
	"float":                     descriptorpb.FieldDescriptorProto_TYPE_FLOAT,
	"double":                    descriptorpb.FieldDescriptorProto_TYPE_DOUBLE,
	"string":                    descriptorpb.FieldDescriptorProto_TYPE_STRING,
	"bool":                      descriptorpb.FieldDescriptorProto_TYPE_BOOL,
	"bytes":                     descriptorpb.FieldDescriptorProto_TYPE_BYTES,
	"google.protobuf.Timestamp": descriptorpb.FieldDescriptorProto_TYPE_MESSAGE,
	"google.protobuf.Duration":  descriptorpb.FieldDescriptorProto_TYPE_MESSAGE,
	"repeated float":            descriptorpb.FieldDescriptorProto_TYPE_FLOAT,
}

// samples mirrors the concrete FieldTypes used by the coltype test. Kept here
// rather than shared because the two packages assert different things about
// them, and a shared table would invite one side's needs to distort the other.
var samples = map[string]dsl.FieldType{
	"bigint":        {Name: "bigint"},
	"int":           {Name: "int"},
	"smallint":      {Name: "smallint"},
	"real":          {Name: "real"},
	"double":        {Name: "double"},
	"boolean":       {Name: "boolean"},
	"text":          {Name: "text"},
	"citext":        {Name: "citext"},
	"jsonb":         {Name: "jsonb"},
	"bytea":         {Name: "bytea"},
	"uuid":          {Name: "uuid"},
	"varchar":       {Name: "varchar"},
	"varchar(N)":    {Name: "varchar", Len: 255},
	"numeric(p, s)": {Name: "numeric", NumP: 12, NumS: 2, HasNumP: true},
	"timestamptz":   {Name: "timestamptz"},
	"date":          {Name: "date"},
	"interval":      {Name: "interval"},
	"vector(N)":     {Name: "vector", VecDim: 3},
}

func TestDispatcherPublishesTheSameWireTypeAsCodegen(t *testing.T) {
	rows, err := dsltypes.Rows()
	if err != nil {
		t.Fatal(err)
	}

	// interval diverges: coltype names google.protobuf.Duration, the
	// dispatcher publishes TYPE_STRING. This is a KNOWN DEFECT, not a
	// deliberate design — an earlier version of this comment called it
	// deliberate, which is exactly how a broken type stays broken.
	//
	// The divergence is not even the whole of it. `interval` does not work on
	// the codegen side either: emitProtoEntity writes
	// `google.protobuf.Duration` into the .proto and imports only
	// timestamp.proto — duration.proto appears nowhere in internal/codegen —
	// so protoc fails with "google.protobuf.Duration is not defined". coltype
	// is also split against itself, GoType saying time.Duration while
	// ScanFragments declares `var v string`.
	//
	// Choosing the wire type is a user-facing decision and is tracked
	// separately. This entry keeps the loop exact for every other type while
	// the choice is open; it must be DELETED, not extended, when interval is
	// settled.
	knownDivergent := map[string]bool{"interval": true}

	seen := map[string]bool{}

	for _, r := range rows {
		seen[r.ATL] = true
		if r.ATL == dsltypes.ArrayRowSpelling {
			continue
		}
		t.Run(r.ATL, func(t *testing.T) {
			ft, ok := samples[r.ATL]
			if !ok {
				t.Fatalf("%s is documented but has no sample here, so nothing "+
					"checks that the dispatcher publishes it correctly", r.ATL)
			}

			protoName, err := coltype.ProtoType(ft)
			if err != nil {
				t.Fatalf("coltype.ProtoType: %v", err)
			}
			want, ok := protoKindForATL[protoName]
			if !ok {
				t.Fatalf("coltype.ProtoType returned %q, which this test has no "+
					"descriptor kind for — add one rather than skipping", protoName)
			}

			fd := &descriptorpb.FieldDescriptorProto{}
			setProtoType(fd, ft)
			if fd.Type == nil {
				t.Fatal("setProtoType left the descriptor type unset")
			}
			if knownDivergent[r.ATL] {
				t.Skipf("documented divergence: codegen says %s, dispatcher says %s",
					protoName, fd.GetType())
			}
			if fd.GetType() != want {
				t.Errorf("dispatcher publishes %s, codegen publishes %s (%q) — a "+
					"client generated from one cannot talk to the other",
					fd.GetType(), want, protoName)
			}
		})
	}

	// The reverse direction, which this file did not have and coltype's
	// equivalent did. The loop above iterates DOC ROWS, so removing a type
	// from the reference page silently stops checking it here while the
	// sample sits unused in the map — and the dispatcher could then drift
	// from codegen for that type with the suite green.
	//
	// dsltypes.Rows()'s row-count floor does not cover this: the page has 19
	// four-cell rows against a floor of 15, so four can go before it fires.
	// The floor guards against the parser breaking entirely; this guards
	// against one row going missing.
	for name := range samples {
		if !seen[name] {
			t.Errorf("sample %q matches no row in %s — either the page dropped "+
				"the type or this map has a typo, and both make the loop above "+
				"quieter than it looks", name, dsltypes.DocPath)
		}
	}
}

// TestDispatcherScansEveryDocumentedType covers the read half. makeScanTarget
// has a silent fallback to scanAny, which stringifies the value through
// fmt.Sprintf into whatever proto field is waiting.
func TestDispatcherScansEveryDocumentedType(t *testing.T) {
	for name, ft := range samples {
		for _, nullable := range []bool{false, true} {
			cm := columnMeta{field: &dsl.Field{Name: "f", Type: ft}, nullable: nullable}
			if st := makeScanTarget(cm); st.tag == scanAny {
				t.Errorf("%s (nullable=%v): makeScanTarget fell through to scanAny, "+
					"so the column is stringified through fmt.Sprintf and the "+
					"client decodes whatever that produced", name, nullable)
			}
		}
	}
}

// TestDispatcherBindsNullForEveryUnsetNullableColumn covers the write half.
//
// The first version of this test was named "ScansAndBinds", its comment named
// bindColumnValue as the hazard, and its body called only makeScanTarget. A
// review found that ten of the eighteen nullable bind arms could be deleted
// with the suite still green — a nullable bigint left unset would have written
// 0 instead of NULL, and every later `WHERE col IS NULL` would have missed the
// row. A test that names a function it never calls is worse than no test,
// because it occupies the space where the real one would go.
//
// The property: for a column whose nullable Go form is a pointer, an UNSET
// proto field must bind something that reaches Postgres as NULL — a sql.NullX
// with Valid false, or a typed nil pointer. The bare getter yields the zero
// value, which is a real 0 or "" in the row.
func TestDispatcherBindsNullForEveryUnsetNullableColumn(t *testing.T) {
	for name, ft := range samples {
		if !strings.HasPrefix(coltype.GoType(ft, false), "*") {
			// []byte, []float32 and arrays carry null as nil already, so the
			// bare getter is the correct answer for them.
			continue
		}
		t.Run(name, func(t *testing.T) {
			e := &dsl.Entity{
				Name: "Probe", Namespace: "probe", Kind: dsl.EntityKindRegular,
				Fields: []dsl.Field{
					{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true, ProtoNumber: 1},
					{Name: "f", Type: ft, ProtoNumber: 2},
				},
			}
			meta := entityMetaFor(e, &dsl.IR{Version: 1})
			fd, err := buildProtoDescriptors(e)
			if err != nil {
				t.Fatalf("buildProtoDescriptors: %v", err)
			}
			resolveProtoDescriptors(meta, fd)

			var cm columnMeta
			for _, c := range meta.columns {
				if c.field.Name == "f" {
					cm = c
				}
			}
			if cm.field == nil {
				t.Fatal("column f is missing from the entity metadata")
			}
			if !cm.nullable {
				t.Fatalf("column f is not nullable, so this case proves nothing")
			}

			// Field left unset: this is a caller omitting it.
			got := bindColumnValue(meta, cm, dynamicpb.NewMessage(meta.msgDesc))
			if !carriesNull(got) {
				t.Errorf("an unset nullable %s binds %#v, which reaches Postgres as "+
					"a value rather than NULL", name, got)
			}
		})
	}
}

// The converse of the test above, and the half that was actually broken.
//
// The test above passes under two very different implementations: one that
// reads proto PRESENCE, and one that merely compares against the zero value.
// Only an EXPLICITLY SET zero separates them — and under the second, `count =
// 0`, `note = ""` and `active = false` all bind SQL NULL. The caller's value is
// not stored, the row reads back as NULL, and nothing anywhere reports a
// problem.
//
// That was the live behaviour: dslFieldToProtoField set Proto3Optional without
// the synthetic oneof it needs, so HasPresence() was false for every nullable
// scalar and Has() degraded to exactly that zero comparison. See
// materializeProto3Optional.
//
// Restricted to types whose proto zero can be set explicitly. Timestamps and
// vectors are message/repeated fields, where "set" and "non-empty" already
// coincide.
func TestDispatcherBindsAnExplicitZeroAsAValue(t *testing.T) {
	zeros := map[string]protoreflect.Value{
		"bigint":     protoreflect.ValueOfInt64(0),
		"int":        protoreflect.ValueOfInt32(0),
		"smallint":   protoreflect.ValueOfInt32(0),
		"real":       protoreflect.ValueOfFloat32(0),
		"double":     protoreflect.ValueOfFloat64(0),
		"boolean":    protoreflect.ValueOfBool(false),
		"text":       protoreflect.ValueOfString(""),
		"citext":     protoreflect.ValueOfString(""),
		"uuid":       protoreflect.ValueOfString(""),
		"varchar":    protoreflect.ValueOfString(""),
		"varchar(N)": protoreflect.ValueOfString(""),
	}
	for name, zero := range zeros {
		ft, ok := samples[name]
		if !ok {
			t.Fatalf("%s is not in samples; the two tables have drifted", name)
		}
		t.Run(name, func(t *testing.T) {
			e := &dsl.Entity{
				Name: "Probe", Namespace: "probe", Kind: dsl.EntityKindRegular,
				Fields: []dsl.Field{
					{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true, ProtoNumber: 1},
					{Name: "f", Type: ft, ProtoNumber: 2},
				},
			}
			meta := entityMetaFor(e, &dsl.IR{Version: 1})
			fd, err := buildProtoDescriptors(e)
			if err != nil {
				t.Fatalf("buildProtoDescriptors: %v", err)
			}
			resolveProtoDescriptors(meta, fd)

			var cm columnMeta
			for _, c := range meta.columns {
				if c.field.Name == "f" {
					cm = c
				}
			}
			if cm.field == nil {
				t.Fatal("column f is missing from the entity metadata")
			}
			if !cm.nullable {
				t.Fatalf("column f is not nullable, so this case proves nothing")
			}

			// The descriptor has to carry presence for the distinction to be
			// expressible at all. Without this the assertion below could only
			// ever fail, and the reason would look like a bind-arm bug rather
			// than a descriptor one.
			pfd := meta.msgDesc.Fields().ByNumber(cm.protoNum)
			if !pfd.HasPresence() {
				t.Fatalf("the dispatcher's descriptor gives nullable %s no presence, "+
					"so an explicit zero is indistinguishable from an unset field on "+
					"the wire and Has() can only compare against the zero value", name)
			}

			msg := dynamicpb.NewMessage(meta.msgDesc)
			msg.Set(pfd, zero)
			got := bindColumnValue(meta, cm, msg)
			if carriesNull(got) {
				t.Errorf("a nullable %s explicitly set to its zero binds %#v, which "+
					"reaches Postgres as NULL. The caller asked for the zero and the "+
					"row will read back as NULL, with no error anywhere", name, got)
			}
		})
	}
}

// carriesNull reports whether a bind value represents SQL NULL: a sql.NullX
// marked invalid, or a nil pointer. Anything else is a real value.
func carriesNull(v any) bool {
	switch x := v.(type) {
	case sql.NullString:
		return !x.Valid
	case sql.NullInt64:
		return !x.Valid
	case sql.NullInt32:
		return !x.Valid
	case sql.NullBool:
		return !x.Valid
	case sql.NullFloat64:
		return !x.Valid
	case sql.NullTime:
		return !x.Valid
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// TestNullFloatScanRespectsTheFieldWidth pins the one place the scan table
// cannot infer the answer from the Go value. `real` and `double` share a scan
// tag because database/sql has no NullFloat32, so the proto field decides the
// width — and setting a float64 on a TYPE_FLOAT field panics inside
// protoreflect rather than converting.
func TestNullFloatScanRespectsTheFieldWidth(t *testing.T) {
	for _, tc := range []struct {
		atl  string
		want protoreflect.Kind
	}{
		{"real", protoreflect.FloatKind},
		{"double", protoreflect.DoubleKind},
	} {
		fd := &descriptorpb.FieldDescriptorProto{}
		setProtoType(fd, dsl.FieldType{Name: tc.atl})
		got := protoreflect.Kind(fd.GetType())
		if got != tc.want {
			t.Errorf("%s publishes %v, want %v — scanNullFloat branches on this "+
				"kind, so the wrong answer here panics on the first null row",
				tc.atl, got, tc.want)
		}
	}
}
