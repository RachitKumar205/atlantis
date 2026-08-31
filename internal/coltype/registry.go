package coltype

import (
	"fmt"
	"sort"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Class groups types whose emitted Go, proto, scan and bind shapes are
// identical. A type joins a Class; it does not carry its own shape.
type Class int

const (
	ClassInt32 Class = iota
	ClassInt64
	ClassFloat32
	ClassFloat64
	ClassString
	ClassBool
	ClassTime
	ClassBytes
	ClassInterval
	ClassVector
)

// param names the argument a Postgres spelling takes. paramNone types render
// sqlName verbatim.
type param int

const (
	paramNone param = iota
	paramLen
	paramNumeric
	paramVecDim
)

// info is one row of the registry: everything the six mapping tables need to
// know about one .atl type.
type info struct {
	class Class
	param param

	// sqlName is the Postgres spelling. For a param type it is a format
	// string; SQLName fills it.
	sqlName string

	// unbounded is the spelling for a paramLen type declared with no length.
	// Len 0 is the unbounded sentinel, so VARCHAR(0) — a column accepting only
	// the empty string — is never rendered.
	unbounded string

	// pred is the stem of the predicate message. codegen spells it
	// <stem>Predicate and the runtime spells it Predicate<stem>.
	//
	// Empty means the type carries no predicate, so it cannot be filtered on
	// or serve as a primary key.
	pred string

	// comparable reports whether Postgres has an equality operator for the
	// type. json, xml, tsvector and the geometric types have none, so a
	// primary key, filter or DISTINCT on one fails at the database.
	comparable bool

	// orderable reports whether Postgres can sort the type, which ORDER BY
	// needs.
	orderable bool

	// textCodec marks a type pgx will not carry in binary format into the Go
	// string its class implies. TextCodecTypes names them, and the pool's
	// AfterConnect hook registers a text codec for each, so the value arrives
	// as Postgres's own text rendering with no cast in the emitted SQL.
	textCodec bool
}

// registry holds one row per .atl type. Adding a type is adding a row here, a
// row on docs/reference/dsl-types.md, and a sample in
// documented_types_test.go, which fails until the first two agree.
var registry = map[string]info{
	"smallint":    {class: ClassInt32, sqlName: "SMALLINT", pred: "Int32", comparable: true, orderable: true},
	"int":         {class: ClassInt32, sqlName: "INTEGER", pred: "Int32", comparable: true, orderable: true},
	"bigint":      {class: ClassInt64, sqlName: "BIGINT", pred: "Int64", comparable: true, orderable: true},
	"real":        {class: ClassFloat32, sqlName: "REAL", pred: "Float", comparable: true, orderable: true},
	"double":      {class: ClassFloat64, sqlName: "DOUBLE PRECISION", pred: "Double", comparable: true, orderable: true},
	"boolean":     {class: ClassBool, sqlName: "BOOLEAN", pred: "Bool", comparable: true, orderable: true},
	"text":        {class: ClassString, sqlName: "TEXT", pred: "String", comparable: true, orderable: true},
	"citext":      {class: ClassString, sqlName: "CITEXT", pred: "String", comparable: true, orderable: true},
	"varchar":     {class: ClassString, sqlName: "VARCHAR(%d)", unbounded: "VARCHAR", param: paramLen, pred: "String", comparable: true, orderable: true},
	"uuid":        {class: ClassString, sqlName: "UUID", pred: "String", comparable: true, orderable: true},
	"numeric":     {class: ClassString, sqlName: "NUMERIC(%d, %d)", unbounded: "NUMERIC", param: paramNumeric, pred: "Numeric", comparable: true, orderable: true},
	"jsonb":       {class: ClassBytes, sqlName: "JSONB", pred: "Bytes", comparable: true, orderable: true},
	"bytea":       {class: ClassBytes, sqlName: "BYTEA", pred: "Bytes", comparable: true, orderable: true},
	"timestamptz": {class: ClassTime, sqlName: "TIMESTAMPTZ", pred: "Timestamp", comparable: true, orderable: true},
	"date":        {class: ClassTime, sqlName: "DATE", pred: "Timestamp", comparable: true, orderable: true},
	"interval":    {class: ClassInterval, sqlName: "INTERVAL", comparable: true, orderable: true},
	"vector":      {class: ClassVector, sqlName: "vector(%d)", param: paramVecDim, comparable: true},

	// Postgres carries a wall-clock timestamp with no zone. The wire type is
	// google.protobuf.Timestamp, which is UTC-anchored, so a value crossing
	// the boundary is read as UTC.
	"timestamp": {class: ClassTime, sqlName: "TIMESTAMP", pred: "Timestamp", comparable: true, orderable: true},

	// Bare `char` renders CHAR, which Postgres reads as CHAR(1). A CHAR(N)
	// column blank-pads its values to N on write and returns them padded.
	"char": {class: ClassString, sqlName: "CHAR(%d)", unbounded: "CHAR", param: paramLen, pred: "String", comparable: true, orderable: true},

	"time":     {class: ClassString, sqlName: "TIME", pred: "String", comparable: true, orderable: true},
	"timetz":   {class: ClassString, sqlName: "TIMETZ", pred: "String", comparable: true, orderable: true},
	"macaddr":  {class: ClassString, sqlName: "MACADDR", pred: "String", comparable: true, orderable: true},
	"macaddr8": {class: ClassString, sqlName: "MACADDR8", pred: "String", comparable: true, orderable: true},
	"tsquery":  {class: ClassString, sqlName: "TSQUERY", pred: "String", comparable: true, orderable: true},
	"name":     {class: ClassString, sqlName: "NAME", pred: "String", comparable: true, orderable: true},

	// The text form carries the symbol and separators lc_monetary names, so a
	// value written as 1.23 reads back as $1.23 under the C locale.
	"money": {class: ClassString, sqlName: "MONEY", pred: "String", comparable: true, orderable: true},

	// json and xml have no equality operator in Postgres, so a filter, a
	// primary key or an ORDER BY on one fails at the database. jsonb has both
	// and is the type to declare for a new column.
	"json": {class: ClassBytes, sqlName: "JSON"},
	"xml":  {class: ClassString, sqlName: "XML"},

	// Carried by a text codec. Each is a type pgx refuses in binary format
	// into a Go string; the codec makes the value arrive as the text
	// Postgres renders, which is what the wire type says it is.
	"inet":     {class: ClassString, sqlName: "INET", pred: "String", comparable: true, orderable: true, textCodec: true},
	"cidr":     {class: ClassString, sqlName: "CIDR", pred: "String", comparable: true, orderable: true, textCodec: true},
	"bit":      {class: ClassString, sqlName: "BIT(%d)", unbounded: "BIT", param: paramLen, pred: "String", comparable: true, orderable: true, textCodec: true},
	"varbit":   {class: ClassString, sqlName: "BIT VARYING(%d)", unbounded: "BIT VARYING", param: paramLen, pred: "String", comparable: true, orderable: true, textCodec: true},
	"tsvector": {class: ClassString, sqlName: "TSVECTOR", pred: "String", comparable: true, orderable: true, textCodec: true},

	"int4range": {class: ClassString, sqlName: "INT4RANGE", pred: "String", comparable: true, orderable: true, textCodec: true},
	"int8range": {class: ClassString, sqlName: "INT8RANGE", pred: "String", comparable: true, orderable: true, textCodec: true},
	"numrange":  {class: ClassString, sqlName: "NUMRANGE", pred: "String", comparable: true, orderable: true, textCodec: true},
	"tsrange":   {class: ClassString, sqlName: "TSRANGE", pred: "String", comparable: true, orderable: true, textCodec: true},
	"tstzrange": {class: ClassString, sqlName: "TSTZRANGE", pred: "String", comparable: true, orderable: true, textCodec: true},
	"daterange": {class: ClassString, sqlName: "DATERANGE", pred: "String", comparable: true, orderable: true, textCodec: true},

	// Postgres sorts none of these, and defines equality for only five of the
	// seven, so none carries a predicate: a filter or ORDER BY would be
	// DDL-clean and fail at the database.
	"point":   {class: ClassString, sqlName: "POINT", textCodec: true},
	"line":    {class: ClassString, sqlName: "LINE", comparable: true, textCodec: true},
	"lseg":    {class: ClassString, sqlName: "LSEG", comparable: true, textCodec: true},
	"box":     {class: ClassString, sqlName: "BOX", comparable: true, textCodec: true},
	"path":    {class: ClassString, sqlName: "PATH", comparable: true, textCodec: true},
	"polygon": {class: ClassString, sqlName: "POLYGON", textCodec: true},
	"circle":  {class: ClassString, sqlName: "CIRCLE", comparable: true, textCodec: true},
}

// enumInfo is the row an enum column behaves as.
//
// Postgres carries an enum label as text on the wire and pgx binds and scans it
// as a Go string with no codec, so an enum is a string that the database
// constrains. Validity is not re-checked here: an unlisted label is refused by
// Postgres as `invalid input value for enum`.
var enumInfo = info{
	class:      ClassString,
	pred:       "String",
	comparable: true,
	orderable:  true,
}

// lookup returns the row for a scalar type.
func lookup(t dsl.FieldType) (info, bool) {
	if t.Enum {
		return enumInfo, true
	}
	i, ok := registry[t.Name]
	return i, ok
}

// Known reports whether the type is one the registry carries. An array is
// known when its element is.
func Known(t dsl.FieldType) bool {
	if t.Array {
		return t.Elem != nil && Known(*t.Elem)
	}
	if t.Enum {
		return true
	}
	_, ok := registry[t.Name]
	return ok
}

// SQLName renders the Postgres spelling for a column of this type.
//
// Returns an error for a type the registry does not carry, so a lowering miss
// surfaces here rather than as DDL Postgres refuses to parse.
func SQLName(t dsl.FieldType) (string, error) {
	if t.Array {
		if t.Elem == nil {
			return "", fmt.Errorf("array type with no element")
		}
		inner, err := SQLName(*t.Elem)
		if err != nil {
			return "", err
		}
		return inner + "[]", nil
	}
	if t.Enum {
		return "", fmt.Errorf("enum %q has no spelling here; schema.SQLType renders it", t.Name)
	}
	i, ok := lookup(t)
	if !ok {
		return "", fmt.Errorf("unknown type %q", t.Name)
	}
	switch i.param {
	case paramLen:
		if t.Len == 0 {
			return i.unbounded, nil
		}
		return fmt.Sprintf(i.sqlName, t.Len), nil
	case paramNumeric:
		if !t.HasNumP {
			return i.unbounded, nil
		}
		return fmt.Sprintf(i.sqlName, t.NumP, t.NumS), nil
	case paramVecDim:
		return fmt.Sprintf(i.sqlName, t.VecDim), nil
	}
	return i.sqlName, nil
}

// PredicateStem returns the stem of the predicate message for a type, and
// whether it has one. Arrays and vectors carry none.
func PredicateStem(t dsl.FieldType) (string, bool) {
	if t.Array {
		return "", false
	}
	i, ok := lookup(t)
	if !ok || i.pred == "" {
		return "", false
	}
	return i.pred, true
}

// Orderable reports whether the type can sit in an ORDER BY clause.
func Orderable(t dsl.FieldType) bool {
	if t.Array {
		return false
	}
	i, ok := lookup(t)
	return ok && i.orderable
}

// Comparable reports whether Postgres has an equality operator for the type.
func Comparable(t dsl.FieldType) bool {
	if t.Array {
		return t.Elem != nil && Comparable(*t.Elem)
	}
	i, ok := lookup(t)
	return ok && i.comparable
}

// CanKey reports whether a column of this type can be a primary key: the
// server addresses a row by equality on it and publishes a predicate for it.
func CanKey(t dsl.FieldType) bool {
	if t.Array {
		return false
	}
	i, ok := lookup(t)
	return ok && i.comparable && i.pred != ""
}

// Names lists every registered type, for tests that hold the registry to the
// reference page.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	return out
}

// classOf returns the behaviour Class of a scalar type, and whether the type
// is registered.
func classOf(t dsl.FieldType) (Class, bool) {
	if t.Enum {
		return enumInfo.class, true
	}
	i, ok := registry[t.Name]
	return i.class, ok
}

// usesSQLNull reports whether the Class scans a nullable column through a
// database/sql Null wrapper.
func usesSQLNull(c Class) bool {
	switch c {
	case ClassBytes, ClassVector:
		return false
	}
	return true
}

// ClassOf returns the behaviour class of a scalar type, and whether the type
// is registered. Callers outside this package switch on it where they need the
// scan or bind shape rather than the emitted spelling.
func ClassOf(t dsl.FieldType) (Class, bool) {
	return classOf(t)
}

// TextCodecTypes names the Postgres types a connection must carry as text.
//
// pgx refuses each in binary format into a Go string, so a pool registers
// pgtype.TextCodec for them at AfterConnect. Without that a read of such a
// column fails with "cannot scan <type> in binary format into *string" at
// request time, having been clean through parse, plan and apply.
//
// `oid` is absent although it has the same problem. pgx reads oid columns in
// its own type resolution, so replacing that codec changes machinery atlantis
// does not own.
func TextCodecTypes() []string {
	out := make([]string, 0, 18)
	for n, i := range registry {
		if i.textCodec {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// CarriedAsText reports whether a connection must decode this type as text.
//
// An array of such a type cannot be carried at all: pgx resolves the array
// codec from the element's, so a text codec decodes the binary array body as
// characters and the scan returns wire bytes rather than failing.
func CarriedAsText(t dsl.FieldType) bool {
	if t.Array {
		return false
	}
	i, ok := registry[t.Name]
	return ok && i.textCodec
}

// ATLSpelling renders the .atl form of a type, parameters included, and
// reports whether the type has one.
//
// The spelling has to round-trip: a declaration emitted from a live database is
// parsed back, and a `char(6)` column written as bare `char` reads as CHAR(1)
// and reports a type change against the column it came from.
func ATLSpelling(t dsl.FieldType) (string, bool) {
	if t.Array {
		if t.Elem == nil {
			return "", false
		}
		elem, ok := ATLSpelling(*t.Elem)
		if !ok {
			return "", false
		}
		return "[]" + elem, true
	}
	if t.Enum {
		return t.Name, true
	}
	i, ok := registry[t.Name]
	if !ok {
		return "", false
	}
	switch i.param {
	case paramLen:
		if t.Len > 0 {
			return fmt.Sprintf("%s(%d)", t.Name, t.Len), true
		}
		return t.Name, true
	case paramNumeric:
		if t.HasNumP {
			return fmt.Sprintf("%s(%d, %d)", t.Name, t.NumP, t.NumS), true
		}
		return t.Name, true
	case paramVecDim:
		if t.VecDim > 0 {
			return fmt.Sprintf("%s(%d)", t.Name, t.VecDim), true
		}
		// parseType requires the parenthesised dimension for this name, so a
		// bare `vector` does not parse.
		return "", false
	}
	return t.Name, true
}
