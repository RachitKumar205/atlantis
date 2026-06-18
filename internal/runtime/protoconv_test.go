package runtime

import (
	"database/sql"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestTimeToProto_RoundTrip(t *testing.T) {
	// Choose a non-zero UTC time with sub-second precision; timestamppb
	// preserves nanos so the round-trip should be exact.
	in := time.Date(2026, 5, 17, 12, 34, 56, 123_456_789, time.UTC)
	out := ProtoToTime(TimeToProto(in))
	if !out.Equal(in) {
		t.Errorf("TimeToProto round-trip lost precision: in=%v out=%v", in, out)
	}
}

func TestTimePtrToProto_NilStaysNil(t *testing.T) {
	if got := TimePtrToProto(nil); got != nil {
		t.Errorf("TimePtrToProto(nil) = %v, want nil", got)
	}
	if got := ProtoToTimePtr(nil); got != nil {
		t.Errorf("ProtoToTimePtr(nil) = %v, want nil", got)
	}
}

func TestTimePtrToProto_RoundTrip(t *testing.T) {
	in := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	p := TimePtrToProto(&in)
	if p == nil {
		t.Fatal("TimePtrToProto on non-nil produced nil")
	}
	out := ProtoToTimePtr(p)
	if out == nil || !out.Equal(in) {
		t.Errorf("round-trip lost value: in=%v out=%v", in, out)
	}
}

func TestProtoToTime_NilIsZero(t *testing.T) {
	// Nil proto → zero time. Callers should validate before reaching here;
	// the helper picks a safe default so we don't panic.
	if got := ProtoToTime(nil); !got.IsZero() {
		t.Errorf("ProtoToTime(nil) = %v, want zero", got)
	}
}

// ---- nullable scalar bind ----

func TestNullableString(t *testing.T) {
	if got := NullableString(nil); got.Valid {
		t.Errorf("NullableString(nil) should be invalid, got %+v", got)
	}
	s := "hello"
	got := NullableString(&s)
	if !got.Valid || got.String != "hello" {
		t.Errorf("NullableString(&s): got %+v want valid=true string=hello", got)
	}
}

func TestNullableInt32(t *testing.T) {
	if got := NullableInt32(nil); got.Valid {
		t.Errorf("NullableInt32(nil) should be invalid")
	}
	v := int32(42)
	got := NullableInt32(&v)
	if !got.Valid || got.Int32 != 42 {
		t.Errorf("NullableInt32(&42): got %+v", got)
	}
}

func TestNullableInt64(t *testing.T) {
	if got := NullableInt64(nil); got.Valid {
		t.Errorf("NullableInt64(nil) should be invalid")
	}
	v := int64(1 << 40)
	got := NullableInt64(&v)
	if !got.Valid || got.Int64 != 1<<40 {
		t.Errorf("NullableInt64: got %+v", got)
	}
}

func TestNullableBool(t *testing.T) {
	if got := NullableBool(nil); got.Valid {
		t.Errorf("NullableBool(nil) should be invalid")
	}
	v := true
	if got := NullableBool(&v); !got.Valid || !got.Bool {
		t.Errorf("NullableBool(&true): got %+v", got)
	}
	// false (zero value) must still come through as Valid=true — that's
	// the whole point of nullable: distinguish "explicit false" from "absent".
	f := false
	if got := NullableBool(&f); !got.Valid || got.Bool {
		t.Errorf("NullableBool(&false): expected valid=true bool=false, got %+v", got)
	}
}

func TestNullableFloat64(t *testing.T) {
	if got := NullableFloat64(nil); got.Valid {
		t.Errorf("NullableFloat64(nil) should be invalid")
	}
	v := 3.14
	if got := NullableFloat64(&v); !got.Valid || got.Float64 != 3.14 {
		t.Errorf("NullableFloat64: got %+v", got)
	}
}

// ---- nullable scalar scan ----

func TestStringPtrFromNull(t *testing.T) {
	if got := StringPtrFromNull(sql.NullString{}); got != nil {
		t.Errorf("invalid NullString should produce nil, got %v", *got)
	}
	got := StringPtrFromNull(sql.NullString{Valid: true, String: "hi"})
	if got == nil || *got != "hi" {
		t.Errorf("valid NullString should produce *string='hi', got %v", got)
	}
}

func TestPtrFromNull_Integers(t *testing.T) {
	// Compact coverage across the int / bool / float variants. The
	// rules: invalid → nil; valid → pointer to the carried value.
	if Int32PtrFromNull(sql.NullInt32{}) != nil {
		t.Error("invalid NullInt32 should yield nil")
	}
	if v := Int32PtrFromNull(sql.NullInt32{Valid: true, Int32: 7}); v == nil || *v != 7 {
		t.Errorf("Int32PtrFromNull(7): got %v", v)
	}
	if Int64PtrFromNull(sql.NullInt64{}) != nil {
		t.Error("invalid NullInt64 should yield nil")
	}
	if v := Int64PtrFromNull(sql.NullInt64{Valid: true, Int64: -9}); v == nil || *v != -9 {
		t.Errorf("Int64PtrFromNull(-9): got %v", v)
	}
	if BoolPtrFromNull(sql.NullBool{}) != nil {
		t.Error("invalid NullBool should yield nil")
	}
	if v := BoolPtrFromNull(sql.NullBool{Valid: true, Bool: true}); v == nil || !*v {
		t.Errorf("BoolPtrFromNull(true): got %v", v)
	}
	if Float64PtrFromNull(sql.NullFloat64{}) != nil {
		t.Error("invalid NullFloat64 should yield nil")
	}
	if v := Float64PtrFromNull(sql.NullFloat64{Valid: true, Float64: 1.5}); v == nil || *v != 1.5 {
		t.Errorf("Float64PtrFromNull(1.5): got %v", v)
	}
}

// ---- vector ----

func TestVectorToFloat32(t *testing.T) {
	if got := VectorToFloat32(nil); got != nil {
		t.Errorf("nil → nil, got %v", got)
	}
	in := []float32{0.1, 0.2, 0.3}
	out := VectorToFloat32(in)
	if len(out) != 3 || out[0] != 0.1 || out[1] != 0.2 || out[2] != 0.3 {
		t.Errorf("VectorToFloat32 corrupted data: %v", out)
	}
	// Defensive copy — mutating the source must not affect the result.
	in[0] = 999
	if out[0] == 999 {
		t.Errorf("VectorToFloat32 should defensively copy; out alias of in")
	}
}

// ---- shape sanity ----

func TestNullableHelpers_ReturnConcreteSQLTypes(t *testing.T) {
	// Compile-time check that the bind helpers return the exact sql.Null
	// concrete types pgx expects. If a future refactor changes one return
	// type, this test stops compiling — which is the failure mode we want.
	var _ sql.NullString = NullableString(nil)
	var _ sql.NullInt32 = NullableInt32(nil)
	var _ sql.NullInt64 = NullableInt64(nil)
	var _ sql.NullBool = NullableBool(nil)
	var _ sql.NullFloat64 = NullableFloat64(nil)
	_ = TimeToProto(time.Now())
	_ = TimePtrToProto(nil)
	_ = ProtoToTime(nil)
	_ = ProtoToTimePtr(&timestamppb.Timestamp{})
}
