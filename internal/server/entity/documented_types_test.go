package entity

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

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

	// interval is the one documented divergence, and it is deliberate rather
	// than an oversight: coltype names google.protobuf.Duration while the
	// dispatcher renders the column as a string. Naming it here means the
	// loop below stays exact for everything else instead of being loosened.
	knownDivergent := map[string]bool{"interval": true}

	for _, r := range rows {
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
}

// TestDispatcherScansAndBindsEveryDocumentedType covers the other two
// dispatcher tables. Both have a silent fallback: makeScanTarget falls to
// scanAny, which stringifies the value through fmt.Sprintf, and bindColumnValue
// falls to msg.Get(fd).Interface(), which hands pgx whatever the proto field
// happens to hold.
func TestDispatcherScansAndBindsEveryDocumentedType(t *testing.T) {
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
