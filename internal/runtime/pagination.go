// Keyset cursor encoding for the QueryX surface.
//
// A cursor pins the (orderColumns..., PK) coordinates of the last row
// of a page so the next page starts strictly after it. Encode owns the
// typed → proto conversion; Decode owns the inverse and validates that
// the cursor was issued for the entity now being queried.
//
// The token format is base64url(raw protobuf of common.v1.PageToken).
// Base64url keeps the cursor URL-safe and avoids padding in the
// canonical form. Tokens are opaque to callers — a caller that
// decodes, mutates, or forges a token has the same effect as omitting
// it: the next call either errors out at Decode or returns a corrupt
// page that the surrounding response object treats as already past.

package runtime

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/common/v1"
)

// ErrInvalidPageToken is returned when a cursor cannot be base64-
// decoded, fails proto unmarshal, or names an entity_id that does not
// match the request. Callers should surface it as codes.InvalidArgument.
var ErrInvalidPageToken = errors.New("runtime: invalid page token")

// PresentOrNil returns v when protoField is present on m, and nil when it is
// absent. Generated cursor extractors call it for every nullable ordering
// column.
//
// A NULL column is scanned into an unset proto field, and codegen emits
// nullable columns with explicit presence, but the getter erases that and
// returns 0 or "" as if the row held a real zero.
//
// The generated code does not test the pointer field directly (`ent.Score !=
// nil`), because the emitted server is only parsed by the test suite and never
// type-checked, so a wrong or renamed struct field would compile in the
// caller's repo and
// nowhere else. Routing through protoreflect means the emitted code names the
// PROTO field, which is the same string codegen wrote into the .proto, and the
// pairing is checkable inside this repo.
//
// An unknown field returns v unchanged. codegen never emits a name it did not
// also declare, which TestEmittedCursorFieldsExistInTheProto holds together.
// Returning nil would turn a codegen slip into every row looking NULL.
func PresentOrNil(m proto.Message, protoField string, v any) any {
	if m == nil {
		return v
	}
	r := m.ProtoReflect()
	fd := r.Descriptor().Fields().ByName(protoreflect.Name(protoField))
	if fd == nil {
		return v
	}
	// HasPresence first: an implicit-presence scalar reports Has() == false for
	// a legitimate zero, so testing Has() alone would report `count = 0` as
	// NULL and page around a value that is really there.
	if fd.HasPresence() && !r.Has(fd) {
		return nil
	}
	return v
}

// EncodePageToken packs the cursor coordinates for one row into a
// base64url-encoded opaque string. values are taken in the order the
// emitted Query<E> handler defines: each requested ORDER BY column
// first, then the entity's primary key as a tiebreaker.
//
// Supported scalar kinds: string, []byte, bool, int / int32 / int64,
// uint / uint32 / uint64, float32 / float64 (cast through string), and
// time.Time. Anything else fails fast — adding a new orderable type
// requires extending the switch here AND in DecodePageToken.
func EncodePageToken(entityID string, values []any) (string, error) {
	if entityID == "" {
		return "", fmt.Errorf("runtime: EncodePageToken: empty entityID")
	}
	tok := &commonpb.PageToken{
		EntityId: entityID,
		Values:   make([]*commonpb.PageTokenValue, 0, len(values)),
	}
	for i, v := range values {
		ptv, err := encodePageTokenValue(v)
		if err != nil {
			return "", fmt.Errorf("runtime: EncodePageToken[%d]: %w", i, err)
		}
		tok.Values = append(tok.Values, ptv)
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(tok)
	if err != nil {
		return "", fmt.Errorf("runtime: EncodePageToken: marshal: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodePageToken is the inverse of EncodePageToken. expectedEntityID
// MUST match the token's entity_id; cross-entity tokens are rejected
// even when the value-shape happens to align, so a cursor issued by
// QueryAccount cannot be smuggled into QueryOrder. An empty token
// returns (nil, nil) — callers branch on len(values) to decide whether
// to inject a cursor predicate.
func DecodePageToken(token, expectedEntityID string) ([]any, error) {
	if token == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("%w: base64: %v", ErrInvalidPageToken, err)
	}
	var tok commonpb.PageToken
	if err := proto.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("%w: unmarshal: %v", ErrInvalidPageToken, err)
	}
	if tok.GetEntityId() != expectedEntityID {
		return nil, fmt.Errorf("%w: entity_id %q does not match %q",
			ErrInvalidPageToken, tok.GetEntityId(), expectedEntityID)
	}
	out := make([]any, 0, len(tok.GetValues()))
	for i, ptv := range tok.GetValues() {
		v, err := decodePageTokenValue(ptv)
		if err != nil {
			return nil, fmt.Errorf("%w: values[%d]: %v", ErrInvalidPageToken, i, err)
		}
		out = append(out, v)
	}
	return out, nil
}

func encodePageTokenValue(v any) (*commonpb.PageTokenValue, error) {
	switch x := v.(type) {
	case string:
		return &commonpb.PageTokenValue{V: &commonpb.PageTokenValue_S{S: x}}, nil
	case []byte:
		return &commonpb.PageTokenValue{V: &commonpb.PageTokenValue_Raw{Raw: x}}, nil
	case bool:
		return &commonpb.PageTokenValue{V: &commonpb.PageTokenValue_B{B: x}}, nil
	case int:
		return &commonpb.PageTokenValue{V: &commonpb.PageTokenValue_I{I: int64(x)}}, nil
	case int32:
		return &commonpb.PageTokenValue{V: &commonpb.PageTokenValue_I{I: int64(x)}}, nil
	case int64:
		return &commonpb.PageTokenValue{V: &commonpb.PageTokenValue_I{I: x}}, nil
	case uint32:
		return &commonpb.PageTokenValue{V: &commonpb.PageTokenValue_I{I: int64(x)}}, nil
	case uint64:
		// Cursor values always come from columns we just scanned out of
		// PG; PG's bigint domain fits in int64, so the truncation guard
		// catches a programmer error (a uint64 in the cursor slice
		// almost certainly indicates a bug upstream), not real data.
		if x > (1<<63)-1 {
			return nil, fmt.Errorf("uint64 %d overflows page-token int64", x)
		}
		return &commonpb.PageTokenValue{V: &commonpb.PageTokenValue_I{I: int64(x)}}, nil
	case float32:
		// Widened to float64 before formatting, and that is the load-bearing
		// part rather than a convenience. Postgres promotes float4 to float8
		// to compare it against the cursor parameter, so the token must carry
		// the value the PROMOTION produces. Formatting at 32-bit precision
		// writes "0.1" for a float4 whose float8 promotion is
		// 0.10000000149011612; the keyset predicate then reads 0.1 < that and
		// hands back the boundary row again, so the caller sees one duplicate
		// row per page forever.
		return &commonpb.PageTokenValue{V: &commonpb.PageTokenValue_Num{
			Num: strconv.FormatFloat(float64(x), 'g', -1, 64)}}, nil
	case float64:
		// 'g' with precision -1 is the shortest form that parses back to the
		// identical float64, so the comparison Postgres performs is against
		// the same bits the row holds. A fixed precision would not be.
		return &commonpb.PageTokenValue{V: &commonpb.PageTokenValue_Num{
			Num: strconv.FormatFloat(x, 'g', -1, 64)}}, nil
	case time.Time:
		return &commonpb.PageTokenValue{V: &commonpb.PageTokenValue_Ts{Ts: timestamppb.New(x)}}, nil
	case *time.Time:
		if x == nil {
			return nil, fmt.Errorf("nil *time.Time in cursor")
		}
		return &commonpb.PageTokenValue{V: &commonpb.PageTokenValue_Ts{Ts: timestamppb.New(*x)}}, nil
	case nil:
		// A NULL ordering key, carried as its own arm.
		//
		// This used to return an error, justified by a comment asserting that
		// "generated handlers strip nullable columns from the order spec".
		// They never did — orderableType rejects only arrays and vectors, and
		// nullable is the DSL default. The error was also unreachable, because
		// the cursor was read through proto getters that return 0 or "" for an
		// unset field, so a NULL arrived as a plausible in-range value instead
		// of as nil. A page landing on a NULL then encoded a cursor no row
		// could be found after, the next page came back empty, and because
		// empty is shorter than the limit no token was emitted at all: every
		// row past the first NULL was unreachable, silently.
		//
		// Ordering is now explicit — ASC NULLS LAST, DESC NULLS FIRST, written
		// into the ORDER BY by OrderByClauseFromKeyset — so a NULL cursor has a
		// defined position and KeysetPredicate can advance past it.
		return &commonpb.PageTokenValue{V: &commonpb.PageTokenValue_Null{Null: true}}, nil
	default:
		return nil, fmt.Errorf("unsupported cursor type %T", v)
	}
}

// KeysetColumn describes one column participating in a keyset cursor:
// its quoted SQL identifier (e.g. `"created_at"`) and whether the
// caller asked for descending order on it. The list is supplied by the
// generated handler from the request's ORDER BY plus the entity's PK
// as the tiebreaker.
type KeysetColumn struct {
	// QuotedIdent is the column ready to drop into SQL — already
	// wrapped in double quotes by the emitter.
	QuotedIdent string
	Desc        bool
	// Nullable reports whether the column may hold NULL.
	//
	// It decides the SHAPE of the predicate, not just its contents, which is
	// why it is a property of the column rather than something inferred from
	// the cursor value. A row-value comparison — `("a","b") > ($1,$2)` —
	// evaluates to NULL for any row whose "a" is NULL, so Postgres drops
	// exactly the rows the next page is trying to reach. That happens whether
	// or not the CURSOR holds a NULL, so reading the cursor is not enough to
	// know the fast path is safe.
	//
	// The PK tiebreaker is never nullable, which is what guarantees the
	// predicate always ends in a strict comparison over a total order.
	Nullable bool
}

// KeysetPredicate renders the WHERE fragment that advances past a
// cursor and the bound argument list to append after the existing
// query args. placeholderStart is the next free $N — the caller has
// already accounted for filter args, the partition arg, etc.
//
// All-ascending columns collapse to a single row-value comparison:
//
//	("a", "b", "pk") > ($1, $2, $3)
//
// PostgreSQL evaluates row-value comparison lexicographically, which
// is exactly the page-advance semantics keyset pagination requires.
// All-descending columns become "<" with the same shape.
//
// Mixed directions defeat row-value comparison because PG has no row
// operator that flips per-column. The fragment expands into the
// canonical nested OR form:
//
//	"a" > $1
//	OR ("a" = $1 AND "b" < $2)
//	OR ("a" = $1 AND "b" = $2 AND "pk" > $3)
//
// which costs one extra branch per column but is index-friendly:
// every disjunct anchors on a left-prefix of the order columns, so a
// composite btree on (a, b, pk) serves it directly.
//
// Returns ("", nil, nil) when cursor is empty — caller skips the
// predicate entirely.
func KeysetPredicate(cols []KeysetColumn, cursor []any, placeholderStart int) (string, []any, error) {
	if len(cursor) == 0 {
		return "", nil, nil
	}
	if len(cols) != len(cursor) {
		return "", nil, fmt.Errorf("runtime: KeysetPredicate: %d cols vs %d cursor values", len(cols), len(cursor))
	}

	allAsc, allDesc, anyNullable := true, true, false
	for i, c := range cols {
		if c.Desc {
			allAsc = false
		} else {
			allDesc = false
		}
		if c.Nullable {
			anyNullable = true
			continue
		}
		// A NULL against a column declared NOT NULL is a contradiction, and
		// left alone it fails in the worst available way. The row-value fast
		// path would bind nil and compare every row against NULL, and the
		// expanded form would render `FALSE` for that position — either way an
		// empty page, no token, and a caller that stops believing it read
		// everything. That is the exact shape this whole file exists to remove,
		// so it is refused rather than rendered.
		//
		// Reachable only through a wiring mistake: a Nullable flag that
		// disagrees with the schema, or a cursor extractor returning nil for a
		// column that cannot hold one. Both are bugs worth a loud failure.
		if cursor[i] == nil {
			return "", nil, fmt.Errorf(
				"runtime: KeysetPredicate: cursor[%d] is NULL but column %s is not nullable",
				i, c.QuotedIdent)
		}
	}

	// A nullable column anywhere forces the expanded form, whatever the cursor
	// holds. Row-value comparison is not merely awkward with NULLs — it is
	// wrong: `("score","id") > ($1,$2)` evaluates to NULL for every row whose
	// "score" is NULL, so those rows are filtered out rather than ordered, and
	// they are precisely the rows the next page exists to return. The result is
	// an empty page, and because empty is shorter than the limit the handler
	// emits no token, so paging stops with rows still unread.
	if anyNullable {
		return nullAwareKeysetPredicate(cols, cursor, placeholderStart)
	}

	args := make([]any, 0, len(cursor))
	if allAsc || allDesc {
		op := ">"
		if allDesc {
			op = "<"
		}
		idents := make([]string, len(cols))
		placeholders := make([]string, len(cols))
		for i, c := range cols {
			idents[i] = c.QuotedIdent
			placeholders[i] = fmt.Sprintf("$%d", placeholderStart+i)
			args = append(args, cursor[i])
		}
		sql := "(" + strings.Join(idents, ", ") + ") " + op + " (" + strings.Join(placeholders, ", ") + ")"
		return sql, args, nil
	}

	// Mixed directions: PG has no row operator that flips per-column,
	// so expand into the nested-OR form. Each outer disjunct
	// anchors on a left-prefix of the order columns, which keeps a
	// composite btree on those columns + the PK index-friendly.
	for i := range cols {
		args = append(args, cursor[i])
	}
	var sb strings.Builder
	sb.WriteByte('(')
	for i := range cols {
		if i > 0 {
			sb.WriteString(" OR ")
		}
		sb.WriteByte('(')
		for j := 0; j < i; j++ {
			if j > 0 {
				sb.WriteString(" AND ")
			}
			fmt.Fprintf(&sb, "%s = $%d", cols[j].QuotedIdent, placeholderStart+j)
		}
		if i > 0 {
			sb.WriteString(" AND ")
		}
		op := ">"
		if cols[i].Desc {
			op = "<"
		}
		fmt.Fprintf(&sb, "%s %s $%d", cols[i].QuotedIdent, op, placeholderStart+i)
		sb.WriteByte(')')
	}
	sb.WriteByte(')')
	return sb.String(), args, nil
}

// nullAwareKeysetPredicate builds the page-advance predicate when any ordering
// column may be NULL.
//
// The same nested-OR expansion the mixed-direction case uses, with both halves
// made null-aware. For column i with cursor value v, under the ordering
// OrderByClauseFromKeyset writes (ASC NULLS LAST, DESC NULLS FIRST):
//
//	equal(c, v)   v non-NULL          c = $n
//	              v NULL              c IS NULL
//
//	after(c, v)   v non-NULL, ASC     (c > $n OR c IS NULL)
//	              v non-NULL, DESC    c < $n
//	              v NULL, ASC         FALSE
//	              v NULL, DESC        c IS NOT NULL
//
// Read them against the ordering and each falls out. Ascending, NULLs sort
// last, so everything after a non-NULL value is either a larger value or a
// NULL — and nothing at all comes after a NULL, because NULLs are the tail.
// Descending, NULLs sort first, so a NULL cursor is followed by every non-NULL
// row, and a non-NULL cursor has already left the NULLs behind.
//
// `after` yields FALSE for a NULL ascending cursor rather than omitting the
// disjunct, because the later columns' equality prefix is built on it. Dropping
// it shortens the chain and lets a row equal on this column but past it on the
// tiebreaker escape the predicate.
//
// The last column is the PK tiebreaker, which is never nullable, so the final
// disjunct is always `... AND pk > $n` — a strict comparison over a total
// order. Every page therefore advances, including one whose entire boundary
// prefix is NULL.
func nullAwareKeysetPredicate(cols []KeysetColumn, cursor []any, placeholderStart int) (string, []any, error) {
	var args []any
	next := placeholderStart

	// Placeholders are allocated as the SQL is written, not by position: a NULL
	// renders as `IS NULL` and consumes no argument, so `placeholderStart+i`
	// stops being the right number for every column after the first NULL.
	// Getting this wrong shifts every later bind and the predicate compares the
	// wrong column against the wrong value — which SQL accepts happily when the
	// types line up.
	equal := func(sb *strings.Builder, c KeysetColumn, v any) {
		if v == nil {
			fmt.Fprintf(sb, "%s IS NULL", c.QuotedIdent)
			return
		}
		fmt.Fprintf(sb, "%s = $%d", c.QuotedIdent, next)
		args = append(args, v)
		next++
	}
	after := func(sb *strings.Builder, c KeysetColumn, v any) {
		switch {
		case v == nil && c.Desc:
			fmt.Fprintf(sb, "%s IS NOT NULL", c.QuotedIdent)
		case v == nil:
			sb.WriteString("FALSE")
		case c.Desc:
			fmt.Fprintf(sb, "%s < $%d", c.QuotedIdent, next)
			args = append(args, v)
			next++
		default:
			// The `OR ... IS NULL` is what reaches the NULL tail. Without it an
			// ascending page stops at the last non-NULL row and every NULL row
			// is unreachable — the defect this function exists for.
			fmt.Fprintf(sb, "(%s > $%d OR %s IS NULL)", c.QuotedIdent, next, c.QuotedIdent)
			args = append(args, v)
			next++
		}
	}

	var sb strings.Builder
	sb.WriteByte('(')
	for i := range cols {
		if i > 0 {
			sb.WriteString(" OR ")
		}
		sb.WriteByte('(')
		for j := 0; j < i; j++ {
			if j > 0 {
				sb.WriteString(" AND ")
			}
			equal(&sb, cols[j], cursor[j])
		}
		if i > 0 {
			sb.WriteString(" AND ")
		}
		after(&sb, cols[i], cursor[i])
		sb.WriteByte(')')
	}
	sb.WriteByte(')')
	return sb.String(), args, nil
}

// OrderByClauseFromKeyset renders the SQL " ORDER BY ..." clause from
// the keyset column list. The empty list yields the empty string —
// callers concatenate directly without an interior conditional.
//
// The NULLS placement is written out even though ASC NULLS LAST and DESC NULLS
// FIRST are PostgreSQL's defaults and a default btree provides exactly these
// orderings, so emitting them changes no plan. Stating them is what lets
// KeysetPredicate rely on them rather than on an ordering nothing in the query
// declares.
//
// Stating it also makes the pair reviewable: flip one of these and the
// corresponding arm in KeysetPredicate is wrong, and the test that walks a page
// boundary over NULLs says so.
func OrderByClauseFromKeyset(cols []KeysetColumn) string {
	if len(cols) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(" ORDER BY ")
	for i, c := range cols {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(c.QuotedIdent)
		if c.Desc {
			sb.WriteString(" DESC NULLS FIRST")
		} else {
			sb.WriteString(" ASC NULLS LAST")
		}
	}
	return sb.String()
}

func decodePageTokenValue(ptv *commonpb.PageTokenValue) (any, error) {
	switch arm := ptv.GetV().(type) {
	case *commonpb.PageTokenValue_S:
		return arm.S, nil
	case *commonpb.PageTokenValue_I:
		return arm.I, nil
	case *commonpb.PageTokenValue_B:
		return arm.B, nil
	case *commonpb.PageTokenValue_Ts:
		if arm.Ts == nil {
			return nil, fmt.Errorf("nil timestamp arm")
		}
		return arm.Ts.AsTime(), nil
	case *commonpb.PageTokenValue_Raw:
		return arm.Raw, nil
	case *commonpb.PageTokenValue_Num:
		return arm.Num, nil
	case *commonpb.PageTokenValue_Null:
		// nil, and distinct from the case below. An absent oneof is a
		// malformed token; this arm is a well-formed token saying the boundary
		// row's value for that column was NULL.
		return nil, nil
	case nil:
		return nil, fmt.Errorf("empty oneof arm")
	default:
		return nil, fmt.Errorf("unknown oneof arm %T", arm)
	}
}
