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
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/common/v1"
)

// ErrInvalidPageToken is returned when a cursor cannot be base64-
// decoded, fails proto unmarshal, or names an entity_id that does not
// match the request. Callers should surface it as codes.InvalidArgument.
var ErrInvalidPageToken = errors.New("runtime: invalid page token")

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
	case time.Time:
		return &commonpb.PageTokenValue{V: &commonpb.PageTokenValue_Ts{Ts: timestamppb.New(x)}}, nil
	case *time.Time:
		if x == nil {
			return nil, fmt.Errorf("nil *time.Time in cursor")
		}
		return &commonpb.PageTokenValue{V: &commonpb.PageTokenValue_Ts{Ts: timestamppb.New(*x)}}, nil
	case nil:
		// NULL ordering keys are not supportable in keyset pagination
		// without an explicit NULLS FIRST / NULLS LAST contract from
		// the caller. Generated handlers strip nullable columns from
		// the order spec; receiving nil here is a bug.
		return nil, fmt.Errorf("nil value in cursor")
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

	allAsc, allDesc := true, true
	for _, c := range cols {
		if c.Desc {
			allAsc = false
		} else {
			allDesc = false
		}
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

// OrderByClauseFromKeyset renders the SQL " ORDER BY ..." clause from
// the keyset column list. The empty list yields the empty string —
// callers concatenate directly without an interior conditional.
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
			sb.WriteString(" DESC")
		} else {
			sb.WriteString(" ASC")
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
	case nil:
		return nil, fmt.Errorf("empty oneof arm")
	default:
		return nil, fmt.Errorf("unknown oneof arm %T", arm)
	}
}
