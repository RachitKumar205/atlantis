// Package-internal conversion helpers shared by generated INSERT/UPDATE/scan code.
package runtime

import (
	"database/sql"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// TimeToProto converts a Go time.Time into the proto timestamp shape.
// Caller is expected to have read the value out of pgx — pgx hands us
// time.Time (in UTC after the AfterConnect hook in internal/storage/pg).
func TimeToProto(t time.Time) *timestamppb.Timestamp {
	return timestamppb.New(t)
}

// TimePtrToProto handles nullable timestamptz columns. nil → nil so the
// proto field stays absent and the wire payload doesn't carry a sentinel.
func TimePtrToProto(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

// ProtoToTime is the inverse for non-null timestamptz columns. A nil
// proto becomes the zero time — callers SHOULD validate required fields
// at the gRPC boundary before this is reached, but we don't panic.
func ProtoToTime(p *timestamppb.Timestamp) time.Time {
	if p == nil {
		return time.Time{}
	}
	return p.AsTime()
}

// ProtoToTimePtr is the inverse for nullable timestamptz columns.
func ProtoToTimePtr(p *timestamppb.Timestamp) *time.Time {
	if p == nil {
		return nil
	}
	t := p.AsTime()
	return &t
}

// There is deliberately NO IntervalToProto / ProtoToInterval helper here.
//
// Both existed briefly and could not work. A helper has to name a concrete Go
// type for atlantis.common.v1.Interval, and there is no single one: each
// generated tree carries its own copy of the message, so this package's
// commonpb.Interval and an emitted server's are different Go types that will
// not assign to each other. The compile fixture in internal/codegen/compilecheck
// reported exactly that, on the first build.
//
// The helpers above are safe because timestamppb and sql.NullX come from shared
// modules — there is one timestamppb.Timestamp. atlantis's own messages have no
// such guarantee, so codegen constructs them inline in the pb package the
// emitted file already imports. See coltype.ScanFragments / coltype.BindExpr.
//
// The dispatcher has its own pair (setIntervalField / intervalFromProto in
// internal/server/entity) because it works in dynamicpb and names no generated
// type at all.

// The generated INSERT / UPDATE handlers receive proto messages where
// nullable scalars are `*T` (proto3 `optional` semantics). pgx wants
// `sql.NullX`-shaped values for nullable columns. These helpers turn
// `*T` → `sql.NullX{Valid: ...}` in one inlined call so the generated
// `bindForXInsert` doesn't repeat the nil-check boilerplate per field.

// NullableString wraps a proto-optional *string into the sql.NullString
// shape pgx wants for nullable columns.
func NullableString(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{Valid: true, String: *p}
}

// NullableInt32 wraps a proto-optional *int32 into sql.NullInt32.
func NullableInt32(p *int32) sql.NullInt32 {
	if p == nil {
		return sql.NullInt32{}
	}
	return sql.NullInt32{Valid: true, Int32: *p}
}

// NullableInt64 wraps a proto-optional *int64 into sql.NullInt64.
func NullableInt64(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Valid: true, Int64: *p}
}

// NullableBool wraps a proto-optional *bool into sql.NullBool.
func NullableBool(p *bool) sql.NullBool {
	if p == nil {
		return sql.NullBool{}
	}
	return sql.NullBool{Valid: true, Bool: *p}
}

// NullableFloat64 wraps a proto-optional *float64 into sql.NullFloat64.
func NullableFloat64(p *float64) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Valid: true, Float64: *p}
}

// NullableFloat32 wraps a proto-optional *float32 into sql.NullFloat64.
//
// database/sql has no NullFloat32, so a nullable `real` column travels as
// float64 in both directions. Widening float32→float64 is exact, and
// Float32PtrFromNull narrows the same value back without loss, so the round
// trip preserves the bits Postgres stores.
func NullableFloat32(p *float32) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Valid: true, Float64: float64(*p)}
}

// On the read path the generated `scanInto<Entity>` receives the result
// of a SELECT and pgx fills `sql.NullX` locals; these helpers turn each
// back into the proto-shaped `*T` the message exposes.

// StringPtrFromNull converts a sql.NullString back to the proto-optional
// *string shape generated messages expose.
func StringPtrFromNull(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

// Int32PtrFromNull converts a sql.NullInt32 back to *int32.
func Int32PtrFromNull(s sql.NullInt32) *int32 {
	if !s.Valid {
		return nil
	}
	v := s.Int32
	return &v
}

// Int64PtrFromNull converts a sql.NullInt64 back to *int64.
func Int64PtrFromNull(s sql.NullInt64) *int64 {
	if !s.Valid {
		return nil
	}
	v := s.Int64
	return &v
}

// BoolPtrFromNull converts a sql.NullBool back to *bool.
func BoolPtrFromNull(s sql.NullBool) *bool {
	if !s.Valid {
		return nil
	}
	v := s.Bool
	return &v
}

// Float64PtrFromNull converts a sql.NullFloat64 back to *float64.
func Float64PtrFromNull(s sql.NullFloat64) *float64 {
	if !s.Valid {
		return nil
	}
	v := s.Float64
	return &v
}

// Float32PtrFromNull converts a sql.NullFloat64 back to *float32, the shape a
// `real` column takes on the wire (proto `float`).
//
// The narrowing is safe for values that came from a `real` column: Postgres
// stores four bytes, pgx widens them to float64 exactly, and this converts
// them back. It is NOT safe in general — a float64 outside float32's range
// becomes ±Inf — which is why this is reached only from a column codegen knows
// is `real`, never from an arbitrary float64.
func Float32PtrFromNull(s sql.NullFloat64) *float32 {
	if !s.Valid {
		return nil
	}
	v := float32(s.Float64)
	return &v
}

// pgvector ships `pgvector.Vector` as the scan target for `vector(N)`
// columns. On the wire we use `repeated float`. The bridge is trivial
// today (both are `[]float32`-shaped) but lives here so the day the
// pgvector wire type changes, the codegen and runtime move in lockstep.

// VectorToFloat32 is identity today — `repeated float` in proto3 is
// `[]float32` in generated Go, and pgvector.Vector unwraps to the same.
// Kept as a named function so a future pgvector type change has one
// place to fix instead of N generated files.
func VectorToFloat32(v []float32) []float32 {
	if v == nil {
		return nil
	}
	out := make([]float32, len(v))
	copy(out, v)
	return out
}
