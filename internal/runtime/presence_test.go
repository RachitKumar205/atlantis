package runtime

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// presenceFixture builds a message with one field of each presence kind:
//
//	explicit  proto3 `optional` — what codegen emits for a nullable column
//	implicit  a bare proto3 scalar — what it emits for NOT NULL
//
// A dynamic message rather than a checked-in .proto because the distinction
// under test is a property of the DESCRIPTOR, and building it here puts both
// kinds side by side in the one place they are compared.
func presenceFixture(t *testing.T) *dynamicpb.Message {
	t.Helper()
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("presence_fixture.proto"),
		Package: proto.String("atlantis.test.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Row"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name:   proto.String("id"),
					Number: proto.Int32(1),
					Label:  &optional,
					Type:   &i64,
				},
				{
					Name:           proto.String("score"),
					Number:         proto.Int32(2),
					Label:          &optional,
					Type:           &i64,
					Proto3Optional: proto.Bool(true),
					OneofIndex:     proto.Int32(0),
				},
			},
			OneofDecl: []*descriptorpb.OneofDescriptorProto{
				{Name: proto.String("_score")},
			},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("build descriptor: %v", err)
	}
	md := fd.Messages().Get(0)

	// Guard the fixture itself. If either field ends up with the presence the
	// test does not expect, every assertion below still runs and proves the
	// wrong thing.
	if md.Fields().ByName("id").HasPresence() {
		t.Fatal("fixture: `id` has explicit presence; it must be a bare proto3 scalar")
	}
	if !md.Fields().ByName("score").HasPresence() {
		t.Fatal("fixture: `score` has no presence; it must be proto3 optional")
	}
	return dynamicpb.NewMessage(md)
}

func TestPresentOrNil_AbsentOptionalBecomesNil(t *testing.T) {
	m := presenceFixture(t)
	// Unset — the state scanRow leaves a field in when the row held NULL.
	if got := PresentOrNil(m, "score", int64(0)); got != nil {
		t.Errorf("PresentOrNil on an unset optional = %#v, want nil. A NULL that "+
			"reaches the cursor as a typed zero names a position no row occupies, "+
			"so the next page comes back empty and paging stops early", got)
	}
}

func TestPresentOrNil_SetOptionalPassesThrough(t *testing.T) {
	m := presenceFixture(t)
	fd := m.Descriptor().Fields().ByName("score")
	m.Set(fd, protoreflect.ValueOfInt64(0))
	// Set to the ZERO value on purpose: a row that really holds 0 must be
	// distinguishable from one that holds NULL, and it is exactly the pair a
	// value-based check cannot tell apart.
	if got := PresentOrNil(m, "score", int64(0)); got != int64(0) {
		t.Errorf("PresentOrNil on an optional explicitly set to 0 = %#v, want int64(0). "+
			"Treating a real zero as NULL pages around a row that exists", got)
	}
}

// The HasPresence guard, stated as its own property.
//
// A bare `Has()` check would be the obvious implementation and it is wrong: an
// implicit-presence scalar reports Has() == false for a legitimate zero, so
// every `id = 0` row would encode a NULL cursor key. This is the mutation the
// plan calls out, and it fails only on a NOT NULL zero-valued key — which no
// other test in the suite constructs.
func TestPresentOrNil_ImplicitPresenceZeroIsNotNull(t *testing.T) {
	m := presenceFixture(t)
	fd := m.Descriptor().Fields().ByName("id")
	if m.Has(fd) {
		t.Fatal("fixture: unset implicit-presence field reports Has() == true")
	}
	if got := PresentOrNil(m, "id", int64(0)); got != int64(0) {
		t.Errorf("PresentOrNil on a NOT NULL column holding 0 = %#v, want int64(0). "+
			"An implicit-presence scalar reports Has() == false for a real zero, so a "+
			"bare Has() check turns `id = 0` into a NULL cursor key", got)
	}
}

// An unknown field name returns the value unchanged rather than nil.
//
// The alternative — nil — would turn a codegen slip into every row looking
// NULL, which is a worse silent outcome than the one PresentOrNil exists to
// remove. Codegen's side of the pairing is asserted by
// TestEmittedCursorFieldsExistInTheProto in internal/codegen.
func TestPresentOrNil_UnknownFieldPassesThrough(t *testing.T) {
	m := presenceFixture(t)
	if got := PresentOrNil(m, "no_such_field", int64(9)); got != int64(9) {
		t.Errorf("PresentOrNil for an unknown field = %#v, want int64(9)", got)
	}
}

func TestPresentOrNil_NilMessagePassesThrough(t *testing.T) {
	if got := PresentOrNil(nil, "score", int64(3)); got != int64(3) {
		t.Errorf("PresentOrNil(nil, ...) = %#v, want int64(3)", got)
	}
}
