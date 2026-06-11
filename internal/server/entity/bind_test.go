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
// that the Shopify import leaves unset.
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

// TestBindColumnValue_EmptyVectorIsNull pins the fix for the Shopify
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
