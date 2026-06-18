package entity

import (
	"testing"

	pgvector "github.com/pgvector/pgvector-go"
	_ "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/common/v1"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// vectorEntity is a minimal entity with one nullable pgvector column,
// modeling the catalog ProductVariant's item_vec/search_vec embeddings
// that the bulk import leaves unset.
func vectorEntity() *dsl.Entity {
	return &dsl.Entity{
		Name:      "Variant",
		Namespace: "consumer",
		Kind:      dsl.EntityKindRegular,
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true, ProtoNumber: 1},
			{Name: "item_vec", Type: dsl.FieldType{Name: "vector", VecDim: 32}, ProtoNumber: 2},
		},
	}
}

func vectorMeta(t *testing.T) *entityMeta {
	t.Helper()
	e := vectorEntity()
	meta := buildEntityMeta(e, &dsl.IR{Version: 1})
	fd, err := buildProtoDescriptors(e)
	if err != nil {
		t.Fatalf("buildProtoDescriptors: %v", err)
	}
	resolveProtoDescriptors(meta, fd)
	if meta.msgDesc == nil {
		t.Fatal("msgDesc not built")
	}
	return meta
}

func vectorCol(t *testing.T, meta *entityMeta) columnMeta {
	t.Helper()
	for _, cm := range meta.insertCols {
		if cm.field.Type.Name == "vector" {
			return cm
		}
	}
	t.Fatal("no vector column in insertCols")
	return columnMeta{}
}

// TestBindColumnValue_EmptyVectorIsNull pins the fix for the Vendor
// import failure: an unset vector column must bind SQL NULL, not an
// empty pgvector — pgvector rejects a 0-dimension value for a
// dimensioned column ("vector must have at least 1 dimension").
func TestBindColumnValue_EmptyVectorIsNull(t *testing.T) {
	meta := vectorMeta(t)
	cm := vectorCol(t, meta)
	msg := dynamicpb.NewMessage(meta.msgDesc) // item_vec left unset

	got := bindColumnValue(meta, cm, msg)
	v, ok := got.(*pgvector.Vector)
	if !ok {
		t.Fatalf("empty vector should bind *pgvector.Vector(nil), got %T", got)
	}
	if v != nil {
		t.Errorf("empty vector should bind a nil pointer (SQL NULL), got %v", v)
	}
}

// TestBindColumnValue_SetVectorBindsValue confirms a populated vector
// still binds a real pgvector value.
func TestBindColumnValue_SetVectorBindsValue(t *testing.T) {
	meta := vectorMeta(t)
	cm := vectorCol(t, meta)
	msg := dynamicpb.NewMessage(meta.msgDesc)

	fd := meta.msgDesc.Fields().ByNumber(cm.protoNum)
	list := msg.Mutable(fd).List()
	list.Append(protoreflect.ValueOfFloat32(1.5))
	list.Append(protoreflect.ValueOfFloat32(2.5))

	got := bindColumnValue(meta, cm, msg)
	v, ok := got.(pgvector.Vector)
	if !ok {
		t.Fatalf("set vector should bind pgvector.Vector, got %T", got)
	}
	if len(v.Slice()) != 2 {
		t.Errorf("vector should have 2 dims, got %d", len(v.Slice()))
	}
}

// TestCustomBindValue_VectorWrapsThroughPgvector pins the custom-query
// counterpart of the entity bind fix: a vector(N) *input* to a custom
// query (e.g. `ORDER BY search_vec <=> $search_vec`) must encode through
// pgvector.NewVector. Before the fix, customBindValue had no "vector"
// case and fell through to msg.Get(fd).String(), stringifying the
// repeated-float list into a literal Postgres rejected with
// "invalid input syntax for type vector".
func TestCustomBindValue_VectorWrapsThroughPgvector(t *testing.T) {
	meta := vectorMeta(t)
	cm := vectorCol(t, meta)
	fd := meta.msgDesc.Fields().ByNumber(cm.protoNum)
	vt := dsl.FieldType{Name: "vector", VecDim: 32}

	// Populated input → a real pgvector.Vector, not a string.
	msg := dynamicpb.NewMessage(meta.msgDesc)
	list := msg.Mutable(fd).List()
	list.Append(protoreflect.ValueOfFloat32(-0.024214512))
	list.Append(protoreflect.ValueOfFloat32(0.25))
	got := customBindValue(msg, fd, vt)
	v, ok := got.(pgvector.Vector)
	if !ok {
		t.Fatalf("vector input should bind pgvector.Vector, got %T (%v)", got, got)
	}
	if len(v.Slice()) != 2 {
		t.Errorf("vector should have 2 dims, got %d", len(v.Slice()))
	}

	// Unset input → SQL NULL (nil *pgvector.Vector), mirroring bindColumnValue.
	empty := dynamicpb.NewMessage(meta.msgDesc)
	gotNull := customBindValue(empty, fd, vt)
	if vp, ok := gotNull.(*pgvector.Vector); !ok || vp != nil {
		t.Errorf("unset vector should bind (*pgvector.Vector)(nil), got %T (%v)", gotNull, gotNull)
	}
}

// TestCustomVectorOutput_ScanRoundTrip pins the output-side counterpart:
// a custom query that SELECTs a vector(N) column must scan it NULL-safely
// and unpack it onto the repeated-float proto field. Before the fix,
// makeCustomScanTarget returned *string for a vector and setCustomProtoField
// tried ValueOfString on a repeated-float field — a panic. NULL must scan
// cleanly (un-embedded search_vec) and leave the field empty.
func TestCustomVectorOutput_ScanRoundTrip(t *testing.T) {
	meta := vectorMeta(t)
	cm := vectorCol(t, meta)
	fd := meta.msgDesc.Fields().ByNumber(cm.protoNum)
	vt := dsl.FieldType{Name: "vector", VecDim: 32}

	// Scan target must be NULL-safe (**pgvector.Vector), not *string.
	target := makeCustomScanTarget(vt)
	pp, ok := target.(**pgvector.Vector)
	if !ok {
		t.Fatalf("vector scan target should be **pgvector.Vector, got %T", target)
	}

	// Value row: pgx allocates and fills the inner Vector.
	vec := pgvector.NewVector([]float32{0.1, -0.2, 0.3})
	*pp = &vec
	msg := dynamicpb.NewMessage(meta.msgDesc)
	setCustomProtoField(msg, fd, vt, target)
	if got := msg.Get(fd).List().Len(); got != 3 {
		t.Errorf("value vector should set 3 floats, got %d", got)
	}

	// NULL row: inner pointer stays nil → field left empty, no panic.
	nullTarget := makeCustomScanTarget(vt)
	nullMsg := dynamicpb.NewMessage(meta.msgDesc)
	setCustomProtoField(nullMsg, fd, vt, nullTarget)
	if got := nullMsg.Get(fd).List().Len(); got != 0 {
		t.Errorf("NULL vector should leave the field empty, got %d floats", got)
	}
}
