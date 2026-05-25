package entity

import (
	"database/sql"
	"time"

	pgvector "github.com/pgvector/pgvector-go"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// bindForInsert returns values ordered to match meta.insertCols
// (the placeholder order in meta.sqlInsert).
func bindForInsert(meta *entityMeta, msg *dynamicpb.Message) []any {
	args := make([]any, 0, len(meta.insertCols))
	for _, cm := range meta.insertCols {
		args = append(args, bindColumnValue(meta, cm, msg))
	}
	return args
}

// bindForUpdate returns SET columns first (meta.updateCols), then PK
// columns (for the WHERE clause placeholders).
func bindForUpdate(meta *entityMeta, msg *dynamicpb.Message) []any {
	args := make([]any, 0, len(meta.updateCols)+len(meta.pkCols))
	for _, cm := range meta.updateCols {
		args = append(args, bindColumnValue(meta, cm, msg))
	}
	for _, cm := range meta.pkCols {
		args = append(args, bindPKValue(meta, cm, msg))
	}
	return args
}

func extractPKValues(meta *entityMeta, msg *dynamicpb.Message) []any {
	args := make([]any, 0, len(meta.pkCols))
	for _, cm := range meta.pkCols {
		args = append(args, bindPKValue(meta, cm, msg))
	}
	return args
}

// bindColumnValue returns the Go value pgx expects for one column,
// mirroring codegen BindExpr but using protoreflect values.
func bindColumnValue(meta *entityMeta, cm columnMeta, msg *dynamicpb.Message) any {
	fd := meta.msgDesc.Fields().ByNumber(cm.protoNum)
	if fd == nil {
		return nil
	}

	t := cm.field.Type

	if t.Array {
		return bindArrayValue(msg, fd)
	}

	switch t.Name {
	case "text", "varchar", "citext", "uuid", "numeric", "interval":
		if !cm.nullable {
			return msg.Get(fd).String()
		}
		if msg.Has(fd) {
			return sql.NullString{Valid: true, String: msg.Get(fd).String()}
		}
		return sql.NullString{}

	case "bigint":
		if !cm.nullable {
			return msg.Get(fd).Int()
		}
		if msg.Has(fd) {
			return sql.NullInt64{Valid: true, Int64: msg.Get(fd).Int()}
		}
		return sql.NullInt64{}

	case "int", "smallint":
		if !cm.nullable {
			return int32(msg.Get(fd).Int())
		}
		if msg.Has(fd) {
			return sql.NullInt32{Valid: true, Int32: int32(msg.Get(fd).Int())}
		}
		return sql.NullInt32{}

	case "boolean":
		if !cm.nullable {
			return msg.Get(fd).Bool()
		}
		if msg.Has(fd) {
			return sql.NullBool{Valid: true, Bool: msg.Get(fd).Bool()}
		}
		return sql.NullBool{}

	case "timestamptz", "date":
		if !cm.nullable {
			return timestampToTime(msg, fd)
		}
		if msg.Has(fd) {
			t := timestampToTime(msg, fd)
			return &t
		}
		return (*time.Time)(nil)

	case "bytea", "jsonb":
		b := msg.Get(fd).Bytes()
		if len(b) == 0 {
			return []byte(nil)
		}
		return b

	case "vector":
		list := msg.Get(fd).List()
		if list.Len() == 0 {
			return pgvector.NewVector(nil)
		}
		floats := make([]float32, list.Len())
		for i := 0; i < list.Len(); i++ {
			floats[i] = float32(list.Get(i).Float())
		}
		return pgvector.NewVector(floats)
	}

	// Fallback.
	return msg.Get(fd).Interface()
}

// bindPKValue uses the non-nullable path (PKs are always NOT NULL).
func bindPKValue(meta *entityMeta, cm columnMeta, msg *dynamicpb.Message) any {
	fd := meta.msgDesc.Fields().ByNumber(cm.protoNum)
	if fd == nil {
		return nil
	}

	t := cm.field.Type
	switch t.Name {
	case "text", "varchar", "citext", "uuid", "numeric":
		return msg.Get(fd).String()
	case "bigint":
		return msg.Get(fd).Int()
	case "int", "smallint":
		return int32(msg.Get(fd).Int())
	case "boolean":
		return msg.Get(fd).Bool()
	case "timestamptz", "date":
		return timestampToTime(msg, fd)
	}
	return msg.Get(fd).Interface()
}

// timestampToTime works with both compiled and dynamic Timestamp messages.
func timestampToTime(msg *dynamicpb.Message, fd protoreflect.FieldDescriptor) time.Time {
	if !msg.Has(fd) {
		return time.Time{}
	}
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
}

func bindArrayValue(msg *dynamicpb.Message, fd protoreflect.FieldDescriptor) any {
	list := msg.Get(fd).List()
	if list.Len() == 0 {
		return nil
	}
	// Determine element kind from the proto field.
	switch fd.Kind() {
	case protoreflect.StringKind:
		out := make([]string, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = list.Get(i).String()
		}
		return out
	case protoreflect.Int32Kind, protoreflect.Sint32Kind:
		out := make([]int32, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = int32(list.Get(i).Int())
		}
		return out
	case protoreflect.Int64Kind, protoreflect.Sint64Kind:
		out := make([]int64, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = list.Get(i).Int()
		}
		return out
	case protoreflect.BoolKind:
		out := make([]bool, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = list.Get(i).Bool()
		}
		return out
	case protoreflect.FloatKind:
		out := make([]float32, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = float32(list.Get(i).Float())
		}
		return out
	}
	// Fallback: []string.
	out := make([]string, list.Len())
	for i := 0; i < list.Len(); i++ {
		out[i] = list.Get(i).String()
	}
	return out
}
