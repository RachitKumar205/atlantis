package entity

import (
	"database/sql"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// scanRow creates typed scan targets per column, calls src.Scan, then
// converts each scanned Go value into a protoreflect.Value on the
// dynamic message.
func scanRow(meta *entityMeta, src interface{ Scan(dest ...any) error }) (*dynamicpb.Message, error) {
	cols := meta.columns
	targets := make([]any, len(cols))
	scanVals := make([]scanTarget, len(cols))

	for i, cm := range cols {
		st := makeScanTarget(cm)
		scanVals[i] = st
		targets[i] = st.ptr
	}

	if err := src.Scan(targets...); err != nil {
		return nil, err
	}

	msg := dynamicpb.NewMessage(meta.msgDesc)
	for i, cm := range cols {
		fd := meta.msgDesc.Fields().ByNumber(cm.protoNum)
		if fd == nil {
			continue
		}
		setProtoFieldFromScan(msg, fd, cm, scanVals[i])
	}
	return msg, nil
}

// scanTarget holds the scan destination pointer and a tag to identify
// the Go type so the post-scan conversion can operate without
// reflection.
type scanTarget struct {
	ptr any
	tag scanTag
}

type scanTag int

const (
	scanString scanTag = iota
	scanNullString
	scanInt64
	scanNullInt64
	scanInt32
	scanNullInt32
	scanBool
	scanNullBool
	scanTime
	scanNullTime
	scanBytes
	scanFloat32
	scanFloat64
	scanNullFloat
	scanVector
	scanNullVector
	scanInterval
	scanFloat32Slice
	scanFloat64Slice
	scanStringSlice
	scanInt32Slice
	scanInt64Slice
	scanBoolSlice
	scanAny
)

func makeScanTarget(cm columnMeta) scanTarget {
	t := cm.field.Type

	if t.Array {
		return makeArrayScanTarget(t)
	}

	switch t.Name {
	case "text", "varchar", "citext", "uuid", "numeric":
		if !cm.nullable {
			v := new(string)
			return scanTarget{ptr: v, tag: scanString}
		}
		v := new(sql.NullString)
		return scanTarget{ptr: v, tag: scanNullString}

	case "bigint":
		if !cm.nullable {
			v := new(int64)
			return scanTarget{ptr: v, tag: scanInt64}
		}
		v := new(sql.NullInt64)
		return scanTarget{ptr: v, tag: scanNullInt64}

	case "int", "smallint":
		if !cm.nullable {
			v := new(int32)
			return scanTarget{ptr: v, tag: scanInt32}
		}
		v := new(sql.NullInt32)
		return scanTarget{ptr: v, tag: scanNullInt32}

	case "real":
		if !cm.nullable {
			v := new(float32)
			return scanTarget{ptr: v, tag: scanFloat32}
		}
		// database/sql has no NullFloat32; the 64-bit form is the scan
		// target and setProtoFieldFromScan narrows to the proto `float`
		// field. Widening float4→float64 and back is exact.
		v := new(sql.NullFloat64)
		return scanTarget{ptr: v, tag: scanNullFloat}

	case "double":
		if !cm.nullable {
			v := new(float64)
			return scanTarget{ptr: v, tag: scanFloat64}
		}
		v := new(sql.NullFloat64)
		return scanTarget{ptr: v, tag: scanNullFloat}

	case "boolean":
		if !cm.nullable {
			v := new(bool)
			return scanTarget{ptr: v, tag: scanBool}
		}
		v := new(sql.NullBool)
		return scanTarget{ptr: v, tag: scanNullBool}

	case "timestamptz", "date":
		if !cm.nullable {
			v := new(time.Time)
			return scanTarget{ptr: v, tag: scanTime}
		}
		v := new(sql.NullTime)
		return scanTarget{ptr: v, tag: scanNullTime}

	case "bytea", "jsonb":
		v := new([]byte)
		return scanTarget{ptr: v, tag: scanBytes}

	case "vector":
		if !cm.nullable {
			v := new(pgvector.Vector)
			return scanTarget{ptr: v, tag: scanVector}
		}
		var v *pgvector.Vector
		return scanTarget{ptr: &v, tag: scanNullVector}

	case "interval":
		// pgtype.Interval for both nullabilities — it carries Valid itself.
		//
		// This used to scan a string, which was consistent with the descriptor
		// when that also said string. Now that both name
		// atlantis.common.v1.Interval, setting a string on a message field
		// would panic inside protoreflect rather than fail a comparison, so
		// the two have to move together.
		v := new(pgtype.Interval)
		return scanTarget{ptr: v, tag: scanInterval}
	}

	// Fallback.
	v := new(any)
	return scanTarget{ptr: v, tag: scanAny}
}

func makeArrayScanTarget(t dsl.FieldType) scanTarget {
	if t.Elem == nil {
		v := new([]string)
		return scanTarget{ptr: v, tag: scanStringSlice}
	}
	switch t.Elem.Name {
	case "text", "varchar", "citext", "uuid", "numeric":
		v := new([]string)
		return scanTarget{ptr: v, tag: scanStringSlice}
	case "int", "smallint":
		v := new([]int32)
		return scanTarget{ptr: v, tag: scanInt32Slice}
	case "bigint":
		v := new([]int64)
		return scanTarget{ptr: v, tag: scanInt64Slice}
	case "boolean":
		v := new([]bool)
		return scanTarget{ptr: v, tag: scanBoolSlice}
	case "real":
		v := new([]float32)
		return scanTarget{ptr: v, tag: scanFloat32Slice}
	case "double":
		v := new([]float64)
		return scanTarget{ptr: v, tag: scanFloat64Slice}
	case "vector":
		v := new([]float32)
		return scanTarget{ptr: v, tag: scanFloat32Slice}
	}
	v := new([]string)
	return scanTarget{ptr: v, tag: scanStringSlice}
}

// setProtoFieldFromScan converts a scanned Go value to a
// protoreflect.Value. Timestamps become sub-messages (seconds + nanos).
func setProtoFieldFromScan(msg *dynamicpb.Message, fd protoreflect.FieldDescriptor, cm columnMeta, st scanTarget) {
	switch st.tag {
	case scanString:
		v := *(st.ptr.(*string))
		msg.Set(fd, protoreflect.ValueOfString(v))

	case scanNullString:
		v := *(st.ptr.(*sql.NullString))
		if v.Valid {
			msg.Set(fd, protoreflect.ValueOfString(v.String))
		}
		// If not valid and field is proto3 optional, leave unset.

	case scanInt64:
		v := *(st.ptr.(*int64))
		msg.Set(fd, protoreflect.ValueOfInt64(v))

	case scanNullInt64:
		v := *(st.ptr.(*sql.NullInt64))
		if v.Valid {
			msg.Set(fd, protoreflect.ValueOfInt64(v.Int64))
		}

	case scanInt32:
		v := *(st.ptr.(*int32))
		msg.Set(fd, protoreflect.ValueOfInt32(v))

	case scanNullInt32:
		v := *(st.ptr.(*sql.NullInt32))
		if v.Valid {
			msg.Set(fd, protoreflect.ValueOfInt32(v.Int32))
		}

	case scanBool:
		v := *(st.ptr.(*bool))
		msg.Set(fd, protoreflect.ValueOfBool(v))

	case scanNullBool:
		v := *(st.ptr.(*sql.NullBool))
		if v.Valid {
			msg.Set(fd, protoreflect.ValueOfBool(v.Bool))
		}

	case scanTime:
		v := *(st.ptr.(*time.Time))
		setTimestampField(msg, fd, v)

	case scanNullTime:
		v := *(st.ptr.(*sql.NullTime))
		if v.Valid {
			setTimestampField(msg, fd, v.Time)
		}

	case scanBytes:
		v := *(st.ptr.(*[]byte))
		if v != nil {
			msg.Set(fd, protoreflect.ValueOfBytes(v))
		}

	case scanFloat32:
		v := *(st.ptr.(*float32))
		msg.Set(fd, protoreflect.ValueOfFloat32(v))

	case scanFloat64:
		v := *(st.ptr.(*float64))
		msg.Set(fd, protoreflect.ValueOfFloat64(v))

	case scanNullFloat:
		// One scan tag serves both `real` and `double`, so the proto field
		// decides the width. Setting a float64 on a TYPE_FLOAT field panics
		// inside protoreflect rather than converting, so this must not
		// guess from the Go value.
		v := *(st.ptr.(*sql.NullFloat64))
		if v.Valid {
			if fd.Kind() == protoreflect.FloatKind {
				msg.Set(fd, protoreflect.ValueOfFloat32(float32(v.Float64)))
			} else {
				msg.Set(fd, protoreflect.ValueOfFloat64(v.Float64))
			}
		}

	case scanInterval:
		// A NULL interval leaves the field unset, so presence carries nullness
		// the same way it does for every other nullable column.
		v := *(st.ptr.(*pgtype.Interval))
		if v.Valid {
			setIntervalField(msg, fd, v)
		}

	case scanVector:
		v := *(st.ptr.(*pgvector.Vector))
		sl := v.Slice()
		setRepeatedFloat32(msg, fd, sl)

	case scanNullVector:
		v := *(st.ptr.(**pgvector.Vector))
		if v != nil {
			sl := (*v).Slice()
			setRepeatedFloat32(msg, fd, sl)
		}

	case scanFloat32Slice:
		v := *(st.ptr.(*[]float32))
		setRepeatedFloat32(msg, fd, v)

	case scanFloat64Slice:
		v := *(st.ptr.(*[]float64))
		list := msg.Mutable(fd).List()
		for _, f := range v {
			list.Append(protoreflect.ValueOfFloat64(f))
		}

	case scanStringSlice:
		v := *(st.ptr.(*[]string))
		list := msg.Mutable(fd).List()
		for _, s := range v {
			list.Append(protoreflect.ValueOfString(s))
		}

	case scanInt32Slice:
		v := *(st.ptr.(*[]int32))
		list := msg.Mutable(fd).List()
		for _, n := range v {
			list.Append(protoreflect.ValueOfInt32(n))
		}

	case scanInt64Slice:
		v := *(st.ptr.(*[]int64))
		list := msg.Mutable(fd).List()
		for _, n := range v {
			list.Append(protoreflect.ValueOfInt64(n))
		}

	case scanBoolSlice:
		v := *(st.ptr.(*[]bool))
		list := msg.Mutable(fd).List()
		for _, b := range v {
			list.Append(protoreflect.ValueOfBool(b))
		}

	case scanAny:
		v := *(st.ptr.(*any))
		if v != nil {
			msg.Set(fd, protoreflect.ValueOfString(fmt.Sprintf("%v", v)))
		}
	}
}

// setTimestampField creates a google.protobuf.Timestamp sub-message on
// the given field descriptor and populates seconds + nanos from the Go
// time.Time. This works with dynamicpb because the Timestamp message
// descriptor was resolved at file-descriptor build time.
func setTimestampField(msg *dynamicpb.Message, fd protoreflect.FieldDescriptor, t time.Time) {
	ts := timestamppb.New(t)
	subMsgDesc := fd.Message()
	if subMsgDesc == nil {
		return
	}
	sub := dynamicpb.NewMessage(subMsgDesc)
	secFD := subMsgDesc.Fields().ByName("seconds")
	nanoFD := subMsgDesc.Fields().ByName("nanos")
	if secFD != nil {
		sub.Set(secFD, protoreflect.ValueOfInt64(ts.GetSeconds()))
	}
	if nanoFD != nil {
		sub.Set(nanoFD, protoreflect.ValueOfInt32(ts.GetNanos()))
	}
	msg.Set(fd, protoreflect.ValueOfMessage(sub))
}

// setIntervalField writes the three components of a Postgres interval into the
// atlantis.common.v1.Interval sub-message, unmodified.
//
// Built field-by-field through protoreflect rather than by constructing a
// commonpb.Interval, because the dispatcher's descriptors are built at run time
// and its messages are dynamicpb — the generated struct is a different type
// from the one this field expects.
func setIntervalField(msg *dynamicpb.Message, fd protoreflect.FieldDescriptor, iv pgtype.Interval) {
	subMsgDesc := fd.Message()
	if subMsgDesc == nil {
		return
	}
	sub := dynamicpb.NewMessage(subMsgDesc)
	if f := subMsgDesc.Fields().ByName("months"); f != nil {
		sub.Set(f, protoreflect.ValueOfInt32(iv.Months))
	}
	if f := subMsgDesc.Fields().ByName("days"); f != nil {
		sub.Set(f, protoreflect.ValueOfInt32(iv.Days))
	}
	if f := subMsgDesc.Fields().ByName("microseconds"); f != nil {
		sub.Set(f, protoreflect.ValueOfInt64(iv.Microseconds))
	}
	msg.Set(fd, protoreflect.ValueOfMessage(sub))
}

// setRepeatedFloat32 appends float32 values to a repeated float field.
func setRepeatedFloat32(msg *dynamicpb.Message, fd protoreflect.FieldDescriptor, vals []float32) {
	list := msg.Mutable(fd).List()
	for _, f := range vals {
		list.Append(protoreflect.ValueOfFloat32(f))
	}
}

// protoValueForCursor extracts a Go value suitable for the pagination
// cursor from a scanned column. Mirrors extractSessionCursor in the
// generated code: the cursor carries the raw Go type (string, int64,
// time.Time, etc.), not proto shapes.
//
// Returns nil for a column that was NULL in the row, which the page token
// carries as its own arm.
//
// # Why HasPresence and not a bare Has
//
// scanRow leaves a field UNSET when the database gave it NULL, and codegen
// emits nullable columns as proto3 `optional`, so presence is a faithful record
// of nullness — but only for fields that HAVE presence. An implicit-presence
// scalar reports Has() == false for a legitimate zero, so checking Has() alone
// would report `id = 0` or `name = ""` as NULL and encode a cursor that skips
// or repeats rows around those values.
//
// Reading through msg.Get() without any presence check is what caused the
// defect this fixes: a NULL arrived as the type's zero value, indistinguishable
// from a real one, and the cursor it produced named a position no row sat at.
func protoValueForCursor(msg *dynamicpb.Message, fd protoreflect.FieldDescriptor, cm columnMeta) any {
	if fd == nil {
		return nil
	}
	if fd.HasPresence() && !msg.Has(fd) {
		return nil
	}
	t := cm.field.Type
	switch t.Name {
	case "text", "varchar", "citext", "uuid", "numeric":
		return msg.Get(fd).String()
	case "interval":
		// Not a cursor coordinate. `interval` is orderable in SQL, but its
		// wire form is now a message and EncodePageToken has no arm for one —
		// it would return "unsupported cursor type", failing the request that
		// tried to page on it rather than issuing a token nothing can decode.
		//
		// Returning nil here would be worse: the null arm encodes fine, so the
		// page would advance past a coordinate that means "no interval" and
		// silently skip rows. Falling through to the unsupported-type error is
		// the loud option, and the one that names the real limitation.
		//
		// Ordering by an interval column still works; only paging on one is
		// refused. Making it pageable needs an Interval arm in the page token,
		// which is a wire change and its own decision.
		return msg.Get(fd).Interface()
	case "bigint":
		return msg.Get(fd).Int()
	case "int", "smallint":
		return int32(msg.Get(fd).Int())
	case "boolean":
		return msg.Get(fd).Bool()
	case "timestamptz", "date":
		sub := msg.Get(fd).Message()
		secFD := sub.Descriptor().Fields().ByName("seconds")
		nanoFD := sub.Descriptor().Fields().ByName("nanos")
		if secFD == nil {
			return time.Time{}
		}
		sec := sub.Get(secFD).Int()
		var nanos int32
		if nanoFD != nil {
			nanos = int32(sub.Get(nanoFD).Int())
		}
		return time.Unix(sec, int64(nanos)).UTC()
	case "bytea", "jsonb":
		return msg.Get(fd).Bytes()
	case "vector":
		list := msg.Get(fd).List()
		out := make([]float32, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = math.Float32frombits(uint32(list.Get(i).Uint()))
		}
		return out
	}
	return msg.Get(fd).Interface()
}
