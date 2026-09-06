package coltype

import (
	"fmt"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// PyType names the Python type a column takes on the generated protobuf
// message, for the annotations the emitted client carries.
//
// There is no nullable form, and that is the difference from GoType rather
// than an omission. Go models a nullable column as *T so absent and zero stay
// apart; protobuf-python's getter returns the field's default for an unset
// field and reports absence through HasField, so the annotation is the same
// either way. A column's nullability reaches Python through presence, not
// through its type.
//
// Returns an error for a type this package does not know. Lowering rejects
// those upstream, so an error here is a codegen bug rather than a schema one.
func PyType(t dsl.FieldType) (string, error) {
	if t.Array {
		if t.Elem == nil {
			return "", fmt.Errorf("array type with no element type")
		}
		inner, err := PyType(*t.Elem)
		if err != nil {
			return "", err
		}
		return "Sequence[" + inner + "]", nil
	}
	c, ok := classOf(t)
	if !ok {
		return "", fmt.Errorf("unsupported type %q for python", t.Name)
	}
	switch c {
	case ClassInt32, ClassInt64:
		return "int", nil
	case ClassFloat32, ClassFloat64:
		return "float", nil
	case ClassString:
		// numeric included. It travels as a decimal string because proto's
		// double and int64 both lose digits it holds, so the Python side
		// receives a str; atlantis_client.to_decimal parses one.
		return "str", nil
	case ClassBool:
		return "bool", nil
	case ClassTime:
		return "timestamp_pb2.Timestamp", nil
	case ClassBytes:
		return "bytes", nil
	case ClassInterval:
		// atlantis.common.v1.Interval, not a timedelta. A Postgres interval
		// carries months, days and microseconds separately, and a timedelta
		// has no months — converting needs a calendar this layer does not
		// have. atlantis_client.interval_to_timedelta raises when months is
		// non-zero rather than guessing a month's length.
		return "interval_pb2.Interval", nil
	case ClassVector:
		return "Sequence[float]", nil
	}
	return "", fmt.Errorf("unsupported type %q for python", t.Name)
}

// PyTypeImport names the generated protobuf module a PyType refers to, and ""
// for a builtin.
//
// Read from the same switch PyType answers from, so a type whose annotation
// names a message cannot reach the emitter without its import.
func PyTypeImport(t dsl.FieldType) string {
	if t.Array {
		if t.Elem == nil {
			return ""
		}
		return PyTypeImport(*t.Elem)
	}
	c, ok := classOf(t)
	if !ok {
		return ""
	}
	switch c {
	case ClassTime:
		return "google.protobuf.timestamp_pb2"
	case ClassInterval:
		return "atlantis.common.v1.interval_pb2"
	}
	return ""
}

// PyJSONType names the Python type a job arg takes after JSON decoding, and
// is a different question from PyType.
//
// Job args are not a protobuf message. They travel as the JSON Go's
// encoding/json produces for the arg struct the Go emitter renders, so this
// mirrors that projection rather than the proto one. Measured against
// encoding/json, not derived:
//
//	int32, int64      1                                        int
//	float32, float64  1.5                                      float
//	string, numeric   "s"                                      str
//	bool              true                                     bool
//	time.Time         "2026-09-07T12:00:00Z"                   str
//	[]byte            "eyJhIjoxfQ=="                           str
//	pgtype.Interval   {"Microseconds":3,"Days":2,"Months":1,   IntervalJSON
//	                   "Valid":true}
//	[]float32         [0.5,1.5]                                Sequence[float]
//
// bytea and jsonb both land on []byte, so both arrive base64-encoded — a
// jsonb arg read as a dict yields the encoded string instead.
//
// notNull is taken because a nullable arg is *T in Go and marshals to JSON
// null, which PyType has no equivalent for: a proto field carries absence
// through presence, and a JSON one carries it in the value.
func PyJSONType(t dsl.FieldType, notNull bool) (string, error) {
	base, err := pyJSONBase(t)
	if err != nil {
		return "", err
	}
	if notNull {
		return base, nil
	}
	return base + " | None", nil
}

func pyJSONBase(t dsl.FieldType) (string, error) {
	if t.Array {
		if t.Elem == nil {
			return "", fmt.Errorf("array type with no element type")
		}
		// Elements of a Go slice are not pointers, so no element is null.
		inner, err := pyJSONBase(*t.Elem)
		if err != nil {
			return "", err
		}
		return "Sequence[" + inner + "]", nil
	}
	c, ok := classOf(t)
	if !ok {
		return "", fmt.Errorf("unsupported type %q for python job args", t.Name)
	}
	switch c {
	case ClassInt32, ClassInt64:
		return "int", nil
	case ClassFloat32, ClassFloat64:
		return "float", nil
	case ClassString:
		return "str", nil
	case ClassBool:
		return "bool", nil
	case ClassTime, ClassBytes:
		return "str", nil
	case ClassInterval:
		return "IntervalJSON", nil
	case ClassVector:
		return "Sequence[float]", nil
	}
	return "", fmt.Errorf("unsupported type %q for python job args", t.Name)
}
