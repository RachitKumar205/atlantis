package entity

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// orderEntity has one of each shape the ORDER BY path branches on: a NOT NULL
// key, a NOT NULL sortable column, a nullable one, and a column that cannot be
// sorted at all.
func orderEntity() *dsl.Entity {
	return &dsl.Entity{
		Name: "Doc", Namespace: "library", Kind: dsl.EntityKindRegular,
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true, ProtoNumber: 1},
			{Name: "title", Type: dsl.FieldType{Name: "text"}, NotNull: true, ProtoNumber: 2},
			{Name: "score", Type: dsl.FieldType{Name: "double"}, ProtoNumber: 3},
			{Name: "embedding", Type: dsl.FieldType{Name: "vector", VecDim: 3}, ProtoNumber: 4},
		},
	}
}

// queryReq builds a Query<E>Request naming the given columns, by proto number,
// paired with a descending flag.
func queryReq(t *testing.T, meta *entityMeta, order ...any) *dynamicpb.Message {
	t.Helper()
	req := dynamicpb.NewMessage(meta.queryRequestDesc)
	if len(order) == 0 {
		return req
	}
	if len(order)%2 != 0 {
		t.Fatalf("queryReq: %d order arguments, want pairs of (proto number, desc)", len(order))
	}
	orderFD := meta.queryRequestDesc.Fields().ByName("order")
	if orderFD == nil {
		t.Fatal("QueryDocRequest has no `order` field: the dispatcher cannot see an ORDER BY at all")
	}
	list := req.Mutable(orderFD).List()
	for i := 0; i < len(order); i += 2 {
		ob := list.NewElement().Message()
		ob.Set(ob.Descriptor().Fields().ByName("field"),
			protoreflect.ValueOfEnum(protoreflect.EnumNumber(order[i].(int))))
		ob.Set(ob.Descriptor().Fields().ByName("desc"),
			protoreflect.ValueOfBool(order[i+1].(bool)))
		list.Append(protoreflect.ValueOfMessage(ob))
	}
	return req
}

// The ORDER BY a caller asks for is the ORDER BY the query runs.
//
// The dispatcher used to declare no `order` field on QueryXRequest, so a
// client's ORDER BY arrived as an unknown field and was discarded. Rows came
// back in primary-key order, the response carried no indication, and the
// documentation showed OrderBy in its opening example.
func TestBuildKeysetCols_OrderIsHonoured(t *testing.T) {
	meta := keysetMeta(t, orderEntity())

	cols, _, err := buildKeysetCols(meta, queryReq(t, meta, 2, true))
	if err != nil {
		t.Fatalf("buildKeysetCols: %v", err)
	}
	got := runtime.OrderByClauseFromKeyset(cols)
	want := ` ORDER BY "title" DESC NULLS FIRST, "id" ASC NULLS LAST`
	if got != want {
		t.Errorf("ORDER BY = %q, want %q", got, want)
	}
}

// A nullable ordering column must reach KeysetPredicate marked nullable.
//
// The flag decides the SHAPE of the predicate, not just its contents: a
// row-value comparison evaluates to NULL for every row whose ordering column is
// NULL, so Postgres drops exactly the rows the next page exists to return.
func TestBuildKeysetCols_NullabilityComesFromTheDeclaration(t *testing.T) {
	meta := keysetMeta(t, orderEntity())

	cols, _, err := buildKeysetCols(meta, queryReq(t, meta, 3, false))
	if err != nil {
		t.Fatalf("buildKeysetCols: %v", err)
	}
	if len(cols) != 2 {
		t.Fatalf("keyset cols = %d, want 2 (score, then the id tiebreaker)", len(cols))
	}
	if !cols[0].Nullable {
		t.Error(`"score" is declared without NOT NULL and reached the keyset as non-nullable; ` +
			"the row-value fast path then filters out every row holding NULL rather than ordering it")
	}
	if cols[1].Nullable {
		t.Error("the id tiebreaker is marked nullable; PRIMARY KEY implies NOT NULL in Postgres")
	}
}

// Ordering by a column that is not orderable is refused, not dropped.
//
// Skipping it would run a different ORDER BY than the one asked for and report
// success, which is the failure this whole change is about.
func TestBuildKeysetCols_UnorderableColumnIsRejected(t *testing.T) {
	meta := keysetMeta(t, orderEntity())

	// `embedding` is a vector: it holds proto number 4 on the entity message
	// and takes no variant in the OrderField enum.
	_, _, err := buildKeysetCols(meta, queryReq(t, meta, 4, false))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ordering by a vector column gave %v, want InvalidArgument", err)
	}
}

// An OrderBy with no field set names no column and is skipped.
//
// UNSPECIFIED is the proto3 zero, so it is what an OrderBy left empty reads as.
// Rejecting it would fail a request that asked for nothing.
func TestBuildKeysetCols_UnspecifiedIsSkipped(t *testing.T) {
	meta := keysetMeta(t, orderEntity())

	cols, _, err := buildKeysetCols(meta, queryReq(t, meta, 0, false))
	if err != nil {
		t.Fatalf("buildKeysetCols: %v", err)
	}
	if len(cols) != 1 || cols[0].QuotedIdent != `"id"` {
		t.Errorf("keyset cols = %v, want just the id tiebreaker", cols)
	}
}

// Every primary-key column the caller did not name is appended.
//
// Ordering by a non-unique column alone is not a total order, so two rows
// sharing the boundary value straddle the page break and the cursor cannot say
// which was already returned.
func TestBuildKeysetCols_CompositePKIsCompletedNotTruncated(t *testing.T) {
	e := &dsl.Entity{
		Name: "Pair", Namespace: "library", Kind: dsl.EntityKindRegular,
		CompositePK: []string{"a", "b"},
		Fields: []dsl.Field{
			{Name: "a", Type: dsl.FieldType{Name: "text"}, NotNull: true, ProtoNumber: 1},
			{Name: "b", Type: dsl.FieldType{Name: "text"}, NotNull: true, ProtoNumber: 2},
			{Name: "v", Type: dsl.FieldType{Name: "int"}, ProtoNumber: 3},
		},
	}
	meta := keysetMeta(t, e)

	// Order by `v`, then by the first half of the key. The second half is
	// still missing, and without it rows sharing (v, a) have no defined order.
	cols, _, err := buildKeysetCols(meta, queryReq(t, meta, 3, false, 1, false))
	if err != nil {
		t.Fatalf("buildKeysetCols: %v", err)
	}
	got := runtime.OrderByClauseFromKeyset(cols)
	if !strings.HasSuffix(got, `"b" ASC NULLS LAST`) {
		t.Errorf("ORDER BY = %q; the second key column is missing, so the ordering is not total "+
			"and the page boundary is ambiguous", got)
	}
	if strings.Count(got, `"a"`) != 1 {
		t.Errorf("ORDER BY = %q; a key column the caller already named was appended a second time", got)
	}
}

// A page token is only meaningful under the ORDER BY it was issued for.
//
// Replayed against one of a different length it carries coordinates for
// columns that are no longer there, and a positional cursor cannot say which.
// The caller changed `order` while paging, so the answer names `page_token`
// and `order` and comes back as InvalidArgument.
func TestCheckCursorArity(t *testing.T) {
	meta := keysetMeta(t, orderEntity())

	// A token issued by a query ordered on title: two coordinates, the title
	// and the id tiebreaker.
	tok, err := runtime.EncodePageToken(meta.entityID, []any{"a", int64(1)})
	if err != nil {
		t.Fatalf("EncodePageToken: %v", err)
	}
	vals, err := runtime.DecodePageToken(tok, meta.entityID)
	if err != nil {
		t.Fatalf("DecodePageToken: %v", err)
	}

	// Replayed with no order at all, which keysets on the id alone.
	shorter, _, err := buildKeysetCols(meta, queryReq(t, meta))
	if err != nil {
		t.Fatalf("buildKeysetCols: %v", err)
	}
	if len(vals) == len(shorter) {
		t.Fatal("fixture: the two orders have the same arity, so this case proves nothing")
	}
	if got := status.Code(checkCursorArity(vals, shorter)); got != codes.InvalidArgument {
		t.Errorf("a cursor of the wrong arity gave %v, want InvalidArgument; the page would "+
			"otherwise advance on coordinates belonging to columns the query is not ordered by", got)
	}

	// The order it was issued under still works.
	same, _, err := buildKeysetCols(meta, queryReq(t, meta, 2, false))
	if err != nil {
		t.Fatalf("buildKeysetCols: %v", err)
	}
	if err := checkCursorArity(vals, same); err != nil {
		t.Errorf("the token was refused under the order it was issued for: %v", err)
	}

	// A first page carries no token, and every query has ordering columns.
	if err := checkCursorArity(nil, same); err != nil {
		t.Errorf("a request with no page_token was refused: %v", err)
	}
}

// The cursor's coordinates come out in the order the ORDER BY names them.
//
// The cursor is positional: KeysetPredicate pairs value i with column i. A
// mismatch compares a title against a score, which Postgres either rejects or,
// where the types line up, answers wrongly.
func TestExtractCursorValues_FollowsTheOrderByPositions(t *testing.T) {
	meta := keysetMeta(t, orderEntity())

	_, ordered, err := buildKeysetCols(meta, queryReq(t, meta, 2, false))
	if err != nil {
		t.Fatalf("buildKeysetCols: %v", err)
	}

	row := dynamicpb.NewMessage(meta.msgDesc)
	row.Set(meta.msgDesc.Fields().ByNumber(1), protoreflect.ValueOfInt64(7))
	row.Set(meta.msgDesc.Fields().ByNumber(2), protoreflect.ValueOfString("hello"))

	vals, err := extractCursorValues(meta, row, ordered)
	if err != nil {
		t.Fatalf("extractCursorValues: %v", err)
	}
	want := []any{"hello", int64(7)}
	if len(vals) != len(want) {
		t.Fatalf("cursor = %v, want %v", vals, want)
	}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("cursor[%d] = %#v, want %#v", i, vals[i], want[i])
		}
	}
}

// A NULL in an ordering column reaches the cursor as nil, not as a zero.
//
// A getter cannot express NULL: it returns 0 or "" for an unset field, which
// names a coordinate no row sits at. The next page then comes back empty and,
// being shorter than the limit, carries no token — so every row past the first
// NULL is unreachable and nothing says so.
func TestExtractCursorValues_NullOrderingColumnIsNil(t *testing.T) {
	meta := keysetMeta(t, orderEntity())

	_, ordered, err := buildKeysetCols(meta, queryReq(t, meta, 3, false))
	if err != nil {
		t.Fatalf("buildKeysetCols: %v", err)
	}

	// `score` left unset: the row holds NULL.
	row := dynamicpb.NewMessage(meta.msgDesc)
	row.Set(meta.msgDesc.Fields().ByNumber(1), protoreflect.ValueOfInt64(7))

	vals, err := extractCursorValues(meta, row, ordered)
	if err != nil {
		t.Fatalf("extractCursorValues: %v", err)
	}
	if vals[0] != nil {
		t.Errorf("cursor[0] = %#v for a NULL score, want nil", vals[0])
	}
}

// A query option the dispatcher does not implement is refused.
//
// Both are declared on QueryXRequest so a caller can set them, and neither is
// served. Leaving them off the descriptor instead would drop them as unknown
// fields and return a full, uneager row set that reads as an answer.
func TestRejectUnsupportedQueryOptions(t *testing.T) {
	meta := keysetMeta(t, orderEntity())

	t.Run("fields", func(t *testing.T) {
		req := dynamicpb.NewMessage(meta.queryRequestDesc)
		fd := meta.queryRequestDesc.Fields().ByName("fields")
		if fd == nil {
			t.Fatal("QueryDocRequest has no `fields`, so a client setting it is silently ignored")
		}
		req.Mutable(fd)
		if got := status.Code(rejectUnsupportedQueryOptions(meta, req)); got != codes.Unimplemented {
			t.Errorf("setting a field mask gave %v, want Unimplemented", got)
		}
	})

	t.Run("includes", func(t *testing.T) {
		req := dynamicpb.NewMessage(meta.queryRequestDesc)
		fd := meta.queryRequestDesc.Fields().ByName("includes")
		if fd == nil {
			t.Fatal("QueryDocRequest has no `includes`, so a client setting it is silently ignored")
		}
		req.Mutable(fd).List().Append(protoreflect.ValueOfEnum(1))
		if got := status.Code(rejectUnsupportedQueryOptions(meta, req)); got != codes.Unimplemented {
			t.Errorf("requesting an include gave %v, want Unimplemented", got)
		}
	})

	t.Run("neither", func(t *testing.T) {
		if err := rejectUnsupportedQueryOptions(meta, dynamicpb.NewMessage(meta.queryRequestDesc)); err != nil {
			t.Errorf("a request setting neither was refused: %v", err)
		}
	})
}
