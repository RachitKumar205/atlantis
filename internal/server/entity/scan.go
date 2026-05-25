package entity

import (
	"database/sql"
	"fmt"
	"math"
	"time"

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
	scanVector
	scanNullVector
	scanFloat32Slice
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
		// Interval is scanned as string.
		if !cm.nullable {
			v := new(string)
			return scanTarget{ptr: v, tag: scanString}
		}
		v := new(sql.NullString)
		return scanTarget{ptr: v, tag: scanNullString}
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
func protoValueForCursor(msg *dynamicpb.Message, fd protoreflect.FieldDescriptor, cm columnMeta) any {
	if fd == nil {
		return nil
	}
	t := cm.field.Type
	switch t.Name {
	case "text", "varchar", "citext", "uuid", "numeric", "interval":
		return msg.Get(fd).String()
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
