package entity

import (
	"testing"

	_ "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/common/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// cursorEntity pairs the two cases that decide protoValueForCursor:
//
//	id     NOT NULL bigint — implicit presence, so an unset field and a
//	       stored 0 are the same wire shape
//	score  nullable double — proto3 optional, so presence is a faithful
//	       record of whether the row held NULL
//
// Both are needed. A test with only nullable fields passes with a bare Has()
// check; a test with only NOT NULL fields passes with no presence check at all.
func cursorEntity() *dsl.Entity {
	return &dsl.Entity{
		Name:      "Doc",
		Namespace: "library",
		Kind:      dsl.EntityKindRegular,
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true, ProtoNumber: 1},
			{Name: "score", Type: dsl.FieldType{Name: "double"}, ProtoNumber: 2},
		},
	}
}

func cursorMeta(t *testing.T) *entityMeta {
	t.Helper()
	return keysetMeta(t, cursorEntity())
}

// keysetMeta is cursorMeta over a caller-supplied entity, for the tests that
// vary the fixture.
func keysetMeta(t *testing.T, e *dsl.Entity) *entityMeta {
	t.Helper()
	meta := entityMetaFor(e, &dsl.IR{Version: 1})
	fd, err := buildProtoDescriptors(e, nil)
	if err != nil {
		t.Fatalf("buildProtoDescriptors: %v", err)
	}
	resolveProtoDescriptors(meta, fd)
	if meta.msgDesc == nil {
		t.Fatal("msgDesc not built")
	}
	return meta
}

func cursorColumn(t *testing.T, meta *entityMeta, name string) (protoreflect.FieldDescriptor, columnMeta) {
	t.Helper()
	for _, cm := range meta.columns {
		if cm.sqlName == name {
			fd := meta.msgDesc.Fields().ByNumber(cm.protoNum)
			if fd == nil {
				t.Fatalf("no proto field for column %q", name)
			}
			return fd, cm
		}
	}
	t.Fatalf("no column metadata for %q", name)
	return nil, columnMeta{}
}

// A column the row held NULL in must reach the cursor as nil.
//
// This is the dispatcher half of #71. scanRow leaves the field unset for a
// NULL, and without the presence check msg.Get() hands back the type's zero —
// indistinguishable from a row that really holds 0.0. The cursor then names a
// coordinate no row sits at, the next page comes back empty, and because empty
// is shorter than the limit no token is emitted at all.
func TestProtoValueForCursor_NullColumnBecomesNil(t *testing.T) {
	meta := cursorMeta(t)
	fd, cm := cursorColumn(t, meta, "score")
	msg := dynamicpb.NewMessage(meta.msgDesc)
	// Left unset — exactly what scanRow does for a NULL.

	if got := protoValueForCursor(msg, fd, cm); got != nil {
		t.Errorf("protoValueForCursor on a NULL column = %#v, want nil. "+
			"A NULL that arrives as %[1]T zero puts the next page's cursor at a "+
			"position no row occupies, and every row past the first NULL becomes "+
			"unreachable with no signal to the caller", got)
	}
}

func TestProtoValueForCursor_SetNullableZeroIsNotNull(t *testing.T) {
	meta := cursorMeta(t)
	fd, cm := cursorColumn(t, meta, "score")
	msg := dynamicpb.NewMessage(meta.msgDesc)
	// Explicitly 0.0: a real value that happens to equal the zero. The pair
	// (unset, set-to-zero) is the whole reason presence has to be read rather
	// than the value compared.
	msg.Set(fd, protoreflect.ValueOfFloat64(0))

	got := protoValueForCursor(msg, fd, cm)
	if f, ok := got.(float64); !ok || f != 0 {
		t.Errorf("protoValueForCursor on a nullable column holding 0.0 = %#v, "+
			"want float64(0). Reporting a stored zero as NULL pages around a row "+
			"that exists", got)
	}
}

// The HasPresence guard, on the path that actually uses it.
//
// A NOT NULL bigint has implicit presence, so an unset field and a stored 0
// are the same wire shape and Has() reports false for both. Checking Has()
// alone would turn every `id = 0` row into a NULL cursor key — a plausible
// implementation that no other test in this package would catch, because every
// other fixture uses non-zero keys.
func TestProtoValueForCursor_NotNullZeroKeyIsNotNull(t *testing.T) {
	meta := cursorMeta(t)
	fd, cm := cursorColumn(t, meta, "id")
	msg := dynamicpb.NewMessage(meta.msgDesc)
	msg.Set(fd, protoreflect.ValueOfInt64(0))

	if msg.Has(fd) {
		t.Fatal("fixture: `id` reports Has() == true when set to 0, so this test " +
			"cannot distinguish HasPresence from a bare Has")
	}
	got := protoValueForCursor(msg, fd, cm)
	if n, ok := got.(int64); !ok || n != 0 {
		t.Errorf("protoValueForCursor on a NOT NULL key holding 0 = %#v, want "+
			"int64(0). An implicit-presence scalar reports Has() == false for a "+
			"real zero, so a bare Has() check makes row id=0 encode a NULL key "+
			"and the walk skips or repeats around it", got)
	}
}

// A request naming no order falls back to the primary key, and a PK is NOT
// NULL in Postgres whatever the declaration says.
//
// The tempting source for this flag is columnMeta.nullable, which holds
// schema.IsEffectivelyNullable — true for any column with a DEFAULT, so a
// serial PK would come through as nullable. That is the right answer for the
// write path it was built for and the wrong one here: it would push every
// default query onto the expanded predicate for no reason.
func TestBuildKeysetCols_PKIsNeverNullable(t *testing.T) {
	e := cursorEntity()
	e.Fields[0].Default = &dsl.Default{Kind: dsl.DefaultIRNow}
	meta := keysetMeta(t, e)

	var pk columnMeta
	for _, cm := range meta.columns {
		if cm.sqlName == "id" {
			pk = cm
		}
	}
	if !pk.nullable {
		t.Fatal("fixture: `id` has a default but columnMeta.nullable is false, so " +
			"this test cannot tell the two sources apart")
	}

	cols, _, err := buildKeysetCols(meta, dynamicpb.NewMessage(meta.queryRequestDesc))
	if err != nil {
		t.Fatalf("buildKeysetCols: %v", err)
	}
	if len(cols) != 1 {
		t.Fatalf("keyset cols = %d, want 1", len(cols))
	}
	if cols[0].Nullable {
		t.Error("the PK keyset column is marked nullable. PRIMARY KEY implies " +
			"NOT NULL in Postgres, and this flag looks like it was taken from " +
			"columnMeta.nullable, which reports true for any column with a default")
	}
}

// A keyset column with no matching column metadata is an error, not a shorter
// slice.
//
// Emitting fewer cursor values than there are columns produces a token that
// nothing rejects until the NEXT request decodes it, and the failure that
// surfaces there names KeysetPredicate and points nowhere near the entity whose
// metadata is inconsistent.
func TestExtractCursorValues_UnknownColumnErrors(t *testing.T) {
	meta := cursorMeta(t)
	msg := dynamicpb.NewMessage(meta.msgDesc)

	cols := []orderColumn{{quotedIdent: `"not_a_column"`, protoNum: 404}}
	got, err := extractCursorValues(meta, msg, cols)
	if err == nil {
		t.Fatalf("extractCursorValues returned (%v, nil) for a column with no "+
			"metadata; the arity mismatch would surface a request later", got)
	}
}

// A failed encode must fail the request, not produce an empty token.
//
// The encode's error used to go to `_`. The empty string it left behind is the
// same value the response carries when a page IS the last one, so a caller
// stopped early believing it had read everything — the exact silent-truncation
// shape #71 is about, reached by a second route.
//
// An empty entityID is the reachable trigger: EncodePageToken refuses it,
// because a token with no entity cannot be validated against the entity it is
// later replayed against.
func TestNextPageToken_ReturnsTheEncodeError(t *testing.T) {
	meta := cursorMeta(t)
	meta.entityID = ""
	msg := dynamicpb.NewMessage(meta.msgDesc)

	tok, err := nextPageToken(meta, msg, meta.pkOrderCols)
	if err == nil {
		t.Fatalf("nextPageToken returned (%q, nil) when the encode fails; an "+
			"empty token reads as 'no more pages' and the caller stops with rows "+
			"still unread", tok)
	}
	if tok != "" {
		t.Errorf("nextPageToken returned token %q alongside an error", tok)
	}
}
