// Package coltype maps a DSL column type to its emitted Go, proto,
// scan, and bind representations. Functions are pure: same DSL inputs
// → same string outputs, which keeps codegen deterministic and makes
// table-driven tests an exhaustive safety net for every DSL type.
package coltype

import (
	"fmt"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// GoType maps a DSL type to its Go in-memory representation.
//
// Nullable scalars surface as `*T` so absent and zero-value stay
// distinguishable. Arrays and naturally nullable shapes (`[]byte`,
// `[]float32`) skip the pointer wrap — their nil zero already encodes
// absence, and double-pointing would force a nil check on the outer
// pointer too.
//
// notNull=true forces the unwrapped form regardless of the column's
// NotNull marker.
func GoType(t dsl.FieldType, notNull bool) string {
	if t.Array {
		if t.Elem == nil {
			return "[]any"
		}
		return "[]" + GoType(*t.Elem, true)
	}
	// An unregistered type keeps the `any` fallback so a lowering miss reads
	// as a column of no usable type; toolchainHandles tests for it.
	base := "any"
	if c, ok := classOf(t); ok {
		base = goBase(c)
	}
	switch base {
	// pgtype.Interval joins the naturally-nullable shapes for the same reason
	// []byte does: it carries its own Valid flag, so a pointer would put a
	// second, redundant absence signal in front of one that already exists —
	// and two ways to say "absent" is two ways to disagree.
	case "[]byte", "[]float32", "pgtype.Interval":
		return base
	}
	if !notNull {
		return "*" + base
	}
	return base
}

// goBase names the scan-side Go type for a behaviour Class, before any
// nullable pointer wrap.
//
// pgtype.Interval is the only Go shape holding a Postgres interval without
// converting between months, days and microseconds. A time.Duration here
// disagrees with what ScanFragments declares, and the two halves of one column
// then emit code that does not compile.
func goBase(c Class) string {
	switch c {
	case ClassInt32:
		return "int32"
	case ClassInt64:
		return "int64"
	case ClassFloat32:
		return "float32"
	case ClassFloat64:
		return "float64"
	case ClassString:
		return "string"
	case ClassBool:
		return "bool"
	case ClassTime:
		return "time.Time"
	case ClassBytes:
		return "[]byte"
	case ClassInterval:
		return "pgtype.Interval"
	case ClassVector:
		return "[]float32"
	}
	return "any"
}

// ProtoType maps a DSL type to its protobuf field-type representation
// (without the field number / name).
//
// Returns an error for types unknown to this package. The IR lowering
// pass rejects unknown types upstream, so an error here surfaces a
// codegen-internal bug rather than a user-facing schema issue.
func ProtoType(t dsl.FieldType) (string, error) {
	if t.Array {
		inner, err := ProtoType(*t.Elem)
		if err != nil {
			return "", err
		}
		return "repeated " + inner, nil
	}
	c, ok := classOf(t)
	if !ok {
		return "", fmt.Errorf("unsupported type %q for proto", t.Name)
	}
	switch c {
	case ClassInt32:
		return "int32", nil
	case ClassInt64:
		return "int64", nil
	case ClassFloat32:
		return "float", nil
	case ClassFloat64:
		return "double", nil
	case ClassString:
		// Exact decimals travel as string and are parsed server-side; proto's
		// double and int64 forms both lose digits numeric holds.
		return "string", nil
	case ClassBool:
		return "bool", nil
	case ClassTime:
		// proto3 well-known type covers wall-clock dates too at the wire
		// boundary; the server converts to Postgres `date` on read/write.
		return "google.protobuf.Timestamp", nil
	case ClassBytes:
		return "bytes", nil
	case ClassInterval:
		// Not google.protobuf.Duration. Duration is one magnitude (seconds +
		// nanos); a Postgres interval is three, and collapsing them needs a
		// calendar this layer does not have. See atlantis/common/v1/interval.proto.
		return "atlantis.common.v1.Interval", nil
	case ClassVector:
		return "repeated float", nil
	}
	return "", fmt.Errorf("unsupported type %q for proto", t.Name)
}

// ScanFragments returns three independently-emittable pieces because
// the decl block, the rows.Scan call, and the post-scan assignment
// loop land in different parts of a generated handler. Caller picks
// the local name (no naming scheme imposed) and the protoField LHS.
//
//   - decl:   `var <local> <go-scan-type>` — the rows.Scan target.
//   - target: `&<local>` — passed into rows.Scan's variadic args.
//   - assign: copies <local> into <protoField>, folding in any
//     nullable→pointer or pgvector→[]float32 conversion.
//
// Mirror of BindExpr in the opposite direction.
func ScanFragments(t dsl.FieldType, notNull bool, local, protoField string) (decl, target, assign string) {
	target = "&" + local

	if t.Array {
		elem := "any"
		if t.Elem != nil {
			elem = GoType(*t.Elem, true)
		}
		decl = fmt.Sprintf("var %s []%s", local, elem)
		assign = fmt.Sprintf("%s = %s", protoField, local)
		return
	}

	c, ok := classOf(t)
	if !ok {
		// Defensive fallback so an IR lowering miss surfaces as "scan
		// target is any" rather than a panic at codegen time.
		decl = fmt.Sprintf("var %s any", local)
		assign = fmt.Sprintf("_ = %s // unknown type %s", local, t.Name)
		return
	}

	switch c {
	case ClassInt32:
		if notNull {
			decl = fmt.Sprintf("var %s int32", local)
			assign = fmt.Sprintf("%s = %s", protoField, local)
		} else {
			decl = fmt.Sprintf("var %s sql.NullInt32", local)
			assign = fmt.Sprintf("%s = runtime.Int32PtrFromNull(%s)", protoField, local)
		}
	case ClassInt64:
		if notNull {
			decl = fmt.Sprintf("var %s int64", local)
			assign = fmt.Sprintf("%s = %s", protoField, local)
		} else {
			decl = fmt.Sprintf("var %s sql.NullInt64", local)
			assign = fmt.Sprintf("%s = runtime.Int64PtrFromNull(%s)", protoField, local)
		}
	case ClassFloat32:
		if notNull {
			decl = fmt.Sprintf("var %s float32", local)
			assign = fmt.Sprintf("%s = %s", protoField, local)
		} else {
			// sql.NullFloat32 does not exist, so the nullable scan target is
			// the 64-bit one and the helper narrows on the way out.
			decl = fmt.Sprintf("var %s sql.NullFloat64", local)
			assign = fmt.Sprintf("%s = runtime.Float32PtrFromNull(%s)", protoField, local)
		}
	case ClassFloat64:
		if notNull {
			decl = fmt.Sprintf("var %s float64", local)
			assign = fmt.Sprintf("%s = %s", protoField, local)
		} else {
			decl = fmt.Sprintf("var %s sql.NullFloat64", local)
			assign = fmt.Sprintf("%s = runtime.Float64PtrFromNull(%s)", protoField, local)
		}
	case ClassString:
		if notNull {
			decl = fmt.Sprintf("var %s string", local)
			assign = fmt.Sprintf("%s = %s", protoField, local)
		} else {
			decl = fmt.Sprintf("var %s sql.NullString", local)
			assign = fmt.Sprintf("%s = runtime.StringPtrFromNull(%s)", protoField, local)
		}
	case ClassBool:
		if notNull {
			decl = fmt.Sprintf("var %s bool", local)
			assign = fmt.Sprintf("%s = %s", protoField, local)
		} else {
			decl = fmt.Sprintf("var %s sql.NullBool", local)
			assign = fmt.Sprintf("%s = runtime.BoolPtrFromNull(%s)", protoField, local)
		}
	case ClassTime:
		if notNull {
			decl = fmt.Sprintf("var %s time.Time", local)
			assign = fmt.Sprintf("%s = runtime.TimeToProto(%s)", protoField, local)
		} else {
			// Inline conversion rather than reaching for
			// runtime.TimePtrToProto — that helper takes *time.Time, not
			// sql.NullTime, so going through it would need a second
			// intermediary.
			decl = fmt.Sprintf("var %s sql.NullTime", local)
			assign = fmt.Sprintf(`if %s.Valid {
		%s = runtime.TimeToProto(%s.Time)
	}`, local, protoField, local)
		}
	case ClassInterval:
		// One declaration for both nullabilities: pgtype.Interval carries Valid
		// itself, so the NULL case needs no separate shape.
		//
		// The message is constructed inline rather than through a runtime
		// helper. A helper has to name a concrete Go type for
		// atlantis.common.v1.Interval and there is no single one — every
		// generated tree carries its own copy, so runtime's commonpb.Interval
		// and an emitted server's are different Go types that will not assign
		// to each other. timestamppb is safe because there is exactly one of
		// it. See internal/runtime/protoconv.go.
		decl = fmt.Sprintf("var %s pgtype.Interval", local)
		assign = fmt.Sprintf(`if %s.Valid {
		%s = &commonpb.Interval{Months: %s.Months, Days: %s.Days, Microseconds: %s.Microseconds}
	}`, local, protoField, local, local, local)
	case ClassBytes:
		decl = fmt.Sprintf("var %s []byte", local)
		assign = fmt.Sprintf("%s = %s", protoField, local)
	case ClassVector:
		if notNull {
			decl = fmt.Sprintf("var %s pgvector.Vector", local)
			assign = fmt.Sprintf("%s = runtime.VectorToFloat32(%s.Slice())", protoField, local)
		} else {
			decl = fmt.Sprintf("var %s *pgvector.Vector", local)
			assign = fmt.Sprintf(`if %s != nil {
		%s = runtime.VectorToFloat32(%s.Slice())
	}`, local, protoField, local)
		}
	}
	return
}

// BindExpr returns the Go expression that turns a proto field into
// the value pgx needs at bind. Mirror of ScanFragments.
//
// protoFieldPtr is needed alongside protoGetter because the runtime
// helpers for nullable scalars take *T (proto3's nullable shape)
// rather than the dereferenced getter. Pass "" when the column is
// not-null — the nullable branches won't be reached.
//
// Vectors wrap through pgvector.NewVector so pgx sees the right
// binary format; arrays return the bare getter because pgx scans
// `[]T` directly via the element's scanner.
func BindExpr(t dsl.FieldType, notNull bool, protoGetter, protoFieldPtr string) string {
	if t.Array {
		return protoGetter
	}
	c, ok := classOf(t)
	if !ok {
		return protoGetter
	}
	switch c {
	case ClassInt32:
		if notNull {
			return protoGetter
		}
		return "runtime.NullableInt32(" + protoFieldPtr + ")"
	case ClassInt64:
		if notNull {
			return protoGetter
		}
		return "runtime.NullableInt64(" + protoFieldPtr + ")"
	case ClassFloat32:
		if notNull {
			return protoGetter
		}
		return "runtime.NullableFloat32(" + protoFieldPtr + ")"
	case ClassFloat64:
		if notNull {
			return protoGetter
		}
		return "runtime.NullableFloat64(" + protoFieldPtr + ")"
	case ClassString:
		if notNull {
			return protoGetter
		}
		return "runtime.NullableString(" + protoFieldPtr + ")"
	case ClassBool:
		if notNull {
			return protoGetter
		}
		return "runtime.NullableBool(" + protoFieldPtr + ")"
	case ClassTime:
		if notNull {
			return "runtime.ProtoToTime(" + protoGetter + ")"
		}
		return "runtime.ProtoToTimePtr(" + protoFieldPtr + ")"
	case ClassInterval:
		// Constructed inline for the same reason the scan side is: a runtime
		// helper cannot take atlantis.common.v1.Interval without naming one
		// generated copy of it, and the emitted server has its own.
		//
		// Valid tracks presence, so a nil message binds SQL NULL while a
		// present '0 seconds' binds zero — different values, kept different.
		// `notNull` is unused rather than forgotten: the message's own absence
		// is the signal at both nullabilities.
		return fmt.Sprintf(
			"pgtype.Interval{Months: %s.GetMonths(), Days: %s.GetDays(), "+
				"Microseconds: %s.GetMicroseconds(), Valid: %s != nil}",
			protoGetter, protoGetter, protoGetter, protoGetter)
	case ClassBytes:
		return protoGetter
	case ClassVector:
		return "pgvector.NewVector(" + protoGetter + ")"
	}
	return protoGetter
}

// NeedsPgvector reports whether emitting code for the given type
// requires the github.com/pgvector/pgvector-go import.
//
// The array recursion is defensive — no current type nests vectors
// under an array — but mirrors the GoType/ProtoType recursion so
// import detection can't diverge from emission shape.
func NeedsPgvector(t dsl.FieldType) bool {
	if t.Array {
		if t.Elem == nil {
			return false
		}
		return NeedsPgvector(*t.Elem)
	}
	c, ok := classOf(t)
	return ok && c == ClassVector
}

// NeedsDatabaseSQL reports whether emitting code for the given type
// requires the database/sql import — driven by the sql.NullX scan
// locals on nullable scalar columns. Returns false for shapes whose
// nullable path scans into a non-sql.Null type (arrays, bytea, jsonb,
// vector).
func NeedsDatabaseSQL(t dsl.FieldType, notNull bool) bool {
	if notNull || t.Array {
		return false
	}
	c, ok := classOf(t)
	return ok && usesSQLNull(c)
}
