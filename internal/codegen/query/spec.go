// Package query is the generalized filter→SQL translator that backs every
// generated QueryX handler. One implementation handles every entity's filter
// type via protoreflect — there is no per-entity code here.
//
// Generated handlers stamp a package-level FilterSpec at codegen time and
// pass it into TranslateFilter alongside the request's filter message. The
// translator returns a SQL WHERE fragment + bind args; the handler splices
// it into the baked SELECT prefix.
//
// Safety guarantees:
//   - Every value flows through pgx parameter binding. Only column names
//     (from the IR-known FieldMap) and SQL operators (from typed oneof arms)
//     are concatenated into the SQL string.
//   - Filter tree depth capped at MaxFilterDepth.
//   - in/not_in list size capped at MaxInListSize.
//   - String values length-capped at MaxStringLen.
//   - LIMIT enforcement happens in the calling handler, not here.
package query

// FilterSpec is the codegen-supplied contract that lets the translator turn
// proto-reflective filter fields into SQL column references. One spec per
// entity; stamped into the generated handler package as a package-level var.
type FilterSpec struct {
	// EntityID is the dotted "namespace.Name" identifier (e.g. "consumer.Account").
	// Surfaced in error messages; not used for SQL emission.
	EntityID string

	// TableName is the unqualified SQL table name (e.g. "consumer_account").
	// Reserved for future use by the translator if it needs to emit aliases;
	// today the calling handler bakes the table into its SQL prefix.
	TableName string

	// Fields maps each filter message field name (in proto snake_case) to the
	// SQL column metadata. Composite arms (`and`/`or`/`not`) are NOT keys
	// here — the translator recognizes them by name.
	Fields map[string]FieldSpec
}

// FieldSpec captures the per-field metadata the translator needs.
type FieldSpec struct {
	// Column is the SQL column name as written in the table. Always already
	// safe to splice — the codegen sourced it from the IR, not from any
	// caller input.
	Column string

	// Kind classifies the predicate type the codegen emitted for this field.
	// Drives the per-type SQL operator lookup table.
	Kind PredicateKind

	// HasTrigramIndex is true when the column has a pg_trgm GIN/GIST index.
	// String predicates' contains / suffix / ilike arms emit an
	// X-Atlantis-Slow-Query: true response header when this is false,
	// since they otherwise sequential-scan the table.
	HasTrigramIndex bool
}

// PredicateKind enumerates the predicate proto messages declared in
// atlantis/common/v1/predicates.proto. Translator dispatches on this.
type PredicateKind int

const (
	PredicateUnknown PredicateKind = iota
	PredicateString
	PredicateInt32
	PredicateInt64
	PredicateBool
	PredicateTimestamp
	PredicateBytes
	PredicateNumeric
)

// Safety caps.
const (
	MaxFilterDepth = 10
	MaxInListSize  = 200
	MaxStringLen   = 1024
)
