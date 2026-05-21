package runtime

import (
	"errors"
	"testing"
	"time"
)

func TestPageToken_RoundTrip_Scalars(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 34, 56, 789000000, time.UTC)
	in := []any{
		"abc",
		int64(42),
		true,
		now,
		[]byte{0x00, 0xff, 0x7f},
	}
	tok, err := EncodePageToken("consumer.Account", in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodePageToken(tok, "consumer.Account")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len mismatch: got %d want %d", len(out), len(in))
	}
	if s, _ := out[0].(string); s != "abc" {
		t.Errorf("string round-trip: got %v", out[0])
	}
	if i, _ := out[1].(int64); i != 42 {
		t.Errorf("int64 round-trip: got %v", out[1])
	}
	if b, _ := out[2].(bool); b != true {
		t.Errorf("bool round-trip: got %v", out[2])
	}
	if ts, ok := out[3].(time.Time); !ok || !ts.Equal(now) {
		t.Errorf("time round-trip: got %v want %v", out[3], now)
	}
	if got, ok := out[4].([]byte); !ok || string(got) != string([]byte{0x00, 0xff, 0x7f}) {
		t.Errorf("bytes round-trip: got %v", out[4])
	}
}

func TestPageToken_EmptyReturnsNil(t *testing.T) {
	out, err := DecodePageToken("", "consumer.Account")
	if err != nil {
		t.Errorf("empty token: got err %v, want nil", err)
	}
	if out != nil {
		t.Errorf("empty token: got values %v, want nil", out)
	}
}

func TestPageToken_RejectsCrossEntity(t *testing.T) {
	tok, err := EncodePageToken("consumer.Account", []any{"x"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := DecodePageToken(tok, "consumer.Order"); !errors.Is(err, ErrInvalidPageToken) {
		t.Errorf("expected ErrInvalidPageToken on cross-entity decode, got %v", err)
	}
}

func TestPageToken_RejectsCorruptedBase64(t *testing.T) {
	if _, err := DecodePageToken("not-base64!!!", "consumer.Account"); !errors.Is(err, ErrInvalidPageToken) {
		t.Errorf("expected ErrInvalidPageToken on bad base64, got %v", err)
	}
}

func TestPageToken_RejectsCorruptedProto(t *testing.T) {
	// Valid base64url decoding into garbage proto bytes.
	if _, err := DecodePageToken("AQID", "consumer.Account"); !errors.Is(err, ErrInvalidPageToken) {
		t.Errorf("expected ErrInvalidPageToken on garbage proto, got %v", err)
	}
}

func TestPageToken_EncodeRejectsNil(t *testing.T) {
	if _, err := EncodePageToken("consumer.Account", []any{nil}); err == nil {
		t.Errorf("expected error on nil cursor value")
	}
}

func TestPageToken_EncodeRejectsUnsupportedType(t *testing.T) {
	if _, err := EncodePageToken("consumer.Account", []any{struct{ X int }{1}}); err == nil {
		t.Errorf("expected error on unsupported cursor type")
	}
}

func TestPageToken_EncodeRejectsEmptyEntity(t *testing.T) {
	if _, err := EncodePageToken("", []any{"x"}); err == nil {
		t.Errorf("expected error on empty entityID")
	}
}

func TestPageToken_DeterministicAcrossEncodes(t *testing.T) {
	// Two encodes of the same input must produce the same string, so a
	// caller running the same query twice gets a cursor that compares
	// equal — useful for hashing and idempotency checks.
	values := []any{"row-7", int64(99), time.Unix(1700000000, 0).UTC()}
	a, err := EncodePageToken("consumer.Account", values)
	if err != nil {
		t.Fatalf("encode a: %v", err)
	}
	b, err := EncodePageToken("consumer.Account", values)
	if err != nil {
		t.Fatalf("encode b: %v", err)
	}
	if a != b {
		t.Errorf("encode non-deterministic: %q vs %q", a, b)
	}
}

func TestKeysetPredicate_AllAscRowValue(t *testing.T) {
	cols := []KeysetColumn{
		{QuotedIdent: `"created_at"`, Desc: false},
		{QuotedIdent: `"id"`, Desc: false},
	}
	sql, args, err := KeysetPredicate(cols, []any{int64(100), "abc"}, 3)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := `("created_at", "id") > ($3, $4)`
	if sql != want {
		t.Errorf("sql: got %q want %q", sql, want)
	}
	if len(args) != 2 || args[0] != int64(100) || args[1] != "abc" {
		t.Errorf("args: got %v", args)
	}
}

func TestKeysetPredicate_AllDescRowValue(t *testing.T) {
	cols := []KeysetColumn{
		{QuotedIdent: `"created_at"`, Desc: true},
		{QuotedIdent: `"id"`, Desc: true},
	}
	sql, _, err := KeysetPredicate(cols, []any{int64(1), int64(2)}, 1)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := `("created_at", "id") < ($1, $2)`
	if sql != want {
		t.Errorf("sql: got %q want %q", sql, want)
	}
}

func TestKeysetPredicate_MixedDirectionsNestedOR(t *testing.T) {
	// Caller asked ORDER BY created_at DESC, id ASC.
	// The PK tiebreaker is ASC by codegen contract; the second column
	// here represents "id ASC" — for the mixed case, the relevant fact
	// is just that the two columns disagree.
	cols := []KeysetColumn{
		{QuotedIdent: `"created_at"`, Desc: true},
		{QuotedIdent: `"id"`, Desc: false},
	}
	sql, args, err := KeysetPredicate(cols, []any{int64(99), "row-x"}, 5)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := `(("created_at" < $5) OR ("created_at" = $5 AND "id" > $6))`
	if sql != want {
		t.Errorf("sql: got %q want %q", sql, want)
	}
	if len(args) != 2 {
		t.Errorf("args: got %v want 2 elements", args)
	}
}

func TestKeysetPredicate_EmptyCursor(t *testing.T) {
	sql, args, err := KeysetPredicate(nil, nil, 1)
	if err != nil || sql != "" || args != nil {
		t.Errorf("empty cursor: got sql=%q args=%v err=%v", sql, args, err)
	}
}

func TestKeysetPredicate_ArityMismatchErrors(t *testing.T) {
	cols := []KeysetColumn{{QuotedIdent: `"a"`}}
	if _, _, err := KeysetPredicate(cols, []any{1, 2}, 1); err == nil {
		t.Errorf("expected arity-mismatch error")
	}
}

func TestOrderByClauseFromKeyset(t *testing.T) {
	cases := []struct {
		name string
		cols []KeysetColumn
		want string
	}{
		{"empty", nil, ""},
		{"single asc",
			[]KeysetColumn{{QuotedIdent: `"id"`}},
			` ORDER BY "id" ASC`,
		},
		{"single desc",
			[]KeysetColumn{{QuotedIdent: `"created_at"`, Desc: true}},
			` ORDER BY "created_at" DESC`,
		},
		{"two columns mixed",
			[]KeysetColumn{
				{QuotedIdent: `"created_at"`, Desc: true},
				{QuotedIdent: `"id"`, Desc: false},
			},
			` ORDER BY "created_at" DESC, "id" ASC`,
		},
	}
	for _, tc := range cases {
		got := OrderByClauseFromKeyset(tc.cols)
		if got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestKeysetPredicate_PlaceholderOffset(t *testing.T) {
	cols := []KeysetColumn{{QuotedIdent: `"id"`}}
	sql, _, err := KeysetPredicate(cols, []any{int64(42)}, 7)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := `("id") > ($7)`
	if sql != want {
		t.Errorf("offset: got %q want %q", sql, want)
	}
}

func TestPageToken_IntVariantsCoerceToInt64(t *testing.T) {
	tok, err := EncodePageToken("e.E", []any{int(1), int32(2), int64(3), uint32(4), uint64(5)})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodePageToken(tok, "e.E")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []int64{1, 2, 3, 4, 5}
	for i, w := range want {
		if got, _ := out[i].(int64); got != w {
			t.Errorf("idx %d: got %v want %d", i, out[i], w)
		}
	}
}
