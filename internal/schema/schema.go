// Package schema provides pure functions that compute SQL identifiers and
// column lists from the DSL IR. These are the shared building blocks for
// both the codegen emitters (which produce Go source as strings) and the
// runtime server (which executes SQL directly).
//
// This package must not import internal/codegen/ — the dependency flows
// the other direction.
package schema

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/coltype"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// QualifiedTable returns the schema-qualified, double-quoted table name
// for an entity, e.g. `"atlantis"."consumer_account"`.
//
// Honors the `table "schema.table"` modifier when set; otherwise falls
// back to the computed `atlantis.<namespace>_<snake>` form.
func QualifiedTable(e *dsl.Entity) string {
	return QuoteIdent(EntitySchema(e)) + "." + QuoteIdent(EntityPhysicalTable(e))
}

// EntitySchema returns the schema where this entity's table lives.
// `table "schema.table"` overrides the default; bare `table "name"`
// (no schema prefix) lives in `public`; no override at all lives in
// `atlantis`.
func EntitySchema(e *dsl.Entity) string {
	if e.TableName != "" {
		if i := strings.IndexByte(e.TableName, '.'); i >= 0 {
			return e.TableName[:i]
		}
		return "public"
	}
	return "atlantis"
}

// EntityPhysicalTable returns the bare table name (the part inside the
// quotes after the schema dot). Without an override it is the computed
// flat name; with one it is the table portion of the override.
func EntityPhysicalTable(e *dsl.Entity) string {
	if e.TableName != "" {
		if i := strings.IndexByte(e.TableName, '.'); i >= 0 {
			return e.TableName[i+1:]
		}
		return e.TableName
	}
	return TableName(e)
}

// TableName maps an entity to its computed physical table name:
// `<namespace>_<snake_case_name>`.
func TableName(e *dsl.Entity) string {
	return e.Namespace + "_" + SnakeCase(e.Name)
}

// QuoteIdent wraps a SQL identifier in double quotes, escaping any
// embedded double quotes by doubling them.
func QuoteIdent(s string) string {
	if !strings.ContainsAny(s, `"\n`) {
		return `"` + s + `"`
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// FieldColumns returns the names of every field on the entity in
// declaration order — the SELECT list for a full-entity read.
func FieldColumns(e *dsl.Entity) []string {
	out := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		out[i] = f.Name
	}
	return out
}

// InsertColumns lists every column the INSERT statement carries values
// for. Identity and Serial columns are excluded because Postgres
// generates their values.
func InsertColumns(e *dsl.Entity) []string {
	var out []string
	for _, f := range e.Fields {
		if f.Identity || f.Serial {
			continue
		}
		out = append(out, f.Name)
	}
	return out
}

// SQLType maps a DSL field type to its Postgres type string.
//
// The spelling comes from the type registry. A name the registry does not
// carry is upper-cased, which produces DDL Postgres rejects at apply rather
// than a column of the wrong type.
func SQLType(t dsl.FieldType) string {
	// An array whose element was never resolved. Lowering fills Elem, so this
	// is reached only by a hand-built FieldType.
	if t.Array && t.Elem == nil {
		return "text[]"
	}
	if t.Enum {
		return QualifiedEnumName(t.Name)
	}
	if s, err := coltype.SQLName(t); err == nil {
		return s
	}
	return strings.ToUpper(t.Name)
}

// QuoteAll wraps every string in double-quoted identifiers.
func QuoteAll(ids []string) []string {
	out := make([]string, len(ids))
	for i, s := range ids {
		out[i] = QuoteIdent(s)
	}
	return out
}

// DefaultExpr renders a Default in SQL form (with appropriate quoting).
func DefaultExpr(d dsl.Default) string {
	switch d.Kind {
	case dsl.DefaultIRString:
		return "'" + strings.ReplaceAll(d.Str, "'", "''") + "'"
	case dsl.DefaultIRInt:
		return fmt.Sprintf("%d", d.Int)
	case dsl.DefaultIRFloat:
		return strconv.FormatFloat(d.Float, 'g', -1, 64)
	case dsl.DefaultIRBool:
		if d.Bool {
			return "TRUE"
		}
		return "FALSE"
	case dsl.DefaultIRNow:
		return "now()"
	case dsl.DefaultIRRaw:
		return d.Str
	}
	return "NULL"
}

// SnakeCase converts UpperCamelCase to snake_case.
func SnakeCase(s string) string {
	var out []rune
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			prev := rune(s[i-1])
			next := rune(0)
			if i+1 < len(s) {
				next = rune(s[i+1])
			}
			if (prev >= 'a' && prev <= 'z') ||
				(next >= 'a' && next <= 'z' && prev >= 'A' && prev <= 'Z') {
				out = append(out, '_')
			}
		}
		if r >= 'A' && r <= 'Z' {
			r = r - 'A' + 'a'
		}
		out = append(out, r)
	}
	return string(out)
}

// PKColumns returns the fields that form the entity's primary key in
// DSL declaration order. Returns nil if no PK is declared (should not
// happen in a well-formed IR).
func PKColumns(e *dsl.Entity) []*dsl.Field {
	if len(e.CompositePK) > 0 {
		out := make([]*dsl.Field, 0, len(e.CompositePK))
		for _, name := range e.CompositePK {
			f := e.FindField(name)
			if f != nil {
				out = append(out, f)
			}
		}
		return out
	}
	if pk := e.PrimaryField(); pk != nil {
		return []*dsl.Field{pk}
	}
	return nil
}

// IsPKColumn reports whether the named column is part of the entity's
// primary key.
func IsPKColumn(e *dsl.Entity, name string) bool {
	for _, f := range PKColumns(e) {
		if f.Name == name {
			return true
		}
	}
	return false
}

// IsEffectivelyNullable reports whether a field should be treated as
// nullable on the proto wire. A field is nullable when it is not NOT
// NULL, or when it has a declared DEFAULT (so the caller can omit it
// and let the server-side COALESCE fire the default).
func IsEffectivelyNullable(f *dsl.Field) bool {
	if f.Default != nil {
		return true
	}
	return !f.NotNull
}

// ExpiryMechanism names how an entity's expired rows are removed.
type ExpiryMechanism int

const (
	// ExpiryNone: the entity declares no ttl_field.
	ExpiryNone ExpiryMechanism = iota

	// ExpiryDelete: a batched DELETE over rows whose ttl_field has passed.
	// Correct for any entity the sweeper can actually see.
	ExpiryDelete

	// ExpiryDropChunks: TimescaleDB drop_chunks() on the hypertable.
	ExpiryDropChunks

	// ExpiryUnreachable: the entity declares expiry the platform cannot
	// perform. `tide apply` refuses this combination rather than accepting a
	// declaration it would silently not honour.
	ExpiryUnreachable
)

// ExpiryFor reports how an entity's expired rows can be removed.
//
// A partitioned entity cannot be swept with DELETE. The sweeper runs on a
// schedule with no request behind it and binds no tenant, so under FORCE ROW
// LEVEL SECURITY the tenant policy applies to its DELETE even though it owns
// the table: current_partition() is NULL, `tenant = NULL` is NULL, no ctids
// match, and the DELETE succeeds having removed nothing.
//
// Binding a tenant is not an option either. Expiry covers every tenant, so
// there is no single correct value, and enumerating tenants needs the
// cross-tenant read the policy prevents.
//
// drop_chunks avoids this because dropping a chunk is DDL, and row-level
// security filters DML rather than DROP TABLE. It is also O(chunks) rather than
// O(rows).
//
// drop_chunks is Apache-2 licensed, which is what makes it usable here:
// add_retention_policy, the automated scheduler, is TSL-only, and atlantis pins
// the Apache-2 build. Its own sweeper job schedules the call, so the TSL
// scheduler is never needed. See ChunkTimeIntervalMS in internal/dsl/ir.go.
//
// ttl_field must be the time field. drop_chunks selects chunks by the
// hypertable's time dimension, so if ttl_field named another column, a chunk
// whose time range has passed could still hold rows whose ttl_field has not,
// and dropping it would delete live data.
func ExpiryFor(e *dsl.Entity) ExpiryMechanism {
	if e == nil || e.TtlField == "" {
		return ExpiryNone
	}
	if e.Kind == dsl.EntityKindHypertable && e.TtlField == e.TimeField {
		return ExpiryDropChunks
	}
	if e.PartitionField != "" {
		return ExpiryUnreachable
	}
	return ExpiryDelete
}

// PredicateKindForField maps a DSL field type to the query.PredicateKind
// constant. Returns ("", false) when the type is not filterable.
func PredicateKindForField(t dsl.FieldType) (string, bool) {
	stem, ok := coltype.PredicateStem(t)
	if !ok {
		return "", false
	}
	return "Predicate" + stem, true
}

// EnumTypeName maps an enum to its computed Postgres type name,
// `<namespace>_<snake_case_name>`, the same flattening TableName applies to an
// entity.
func EnumTypeName(e *dsl.Enum) string {
	return e.Namespace + "_" + SnakeCase(e.Name)
}

// QualifiedEnum returns the schema-qualified, double-quoted type name for an
// enum, e.g. `"atlantis"."app_mood"`.
//
// Enums live in the atlantis schema whatever `table` override the entities
// using them carry: the type is atlantis's to create and drop, and putting it
// beside a table in a schema atlantis does not own would leave it behind when
// that schema is dropped.
func QualifiedEnum(e *dsl.Enum) string {
	return QuoteIdent("atlantis") + "." + QuoteIdent(EnumTypeName(e))
}

// QualifiedEnumName renders the Postgres type for an enum ID (`ns.Name`).
//
// Lowering rewrites an enum-typed field to hold the ID, so this is the only
// place the flattening has to agree with EnumTypeName.
func QualifiedEnumName(id string) string {
	ns, name, ok := strings.Cut(id, ".")
	if !ok {
		return QuoteIdent("atlantis") + "." + QuoteIdent(SnakeCase(id))
	}
	return QuoteIdent("atlantis") + "." + QuoteIdent(ns+"_"+SnakeCase(name))
}

// ScreamingSnake turns "AccountOrderField" into "ACCOUNT_ORDER_FIELD".
//
// The rule is buf's ENUM_VALUE_PREFIX heuristic, which the lint compares
// against: insert `_` before an uppercase character when the previous character
// is lowercase or the next one is. That renders `OAuthProvider` as
// `O_AUTH_PROVIDER`, and matching buf is what keeps the emitted proto lint-clean.
func ScreamingSnake(camel string) string {
	rs := []rune(camel)
	var b strings.Builder
	for i, r := range rs {
		if i > 0 && r >= 'A' && r <= 'Z' {
			prevLower := rs[i-1] >= 'a' && rs[i-1] <= 'z'
			nextLower := i+1 < len(rs) && rs[i+1] >= 'a' && rs[i+1] <= 'z'
			if prevLower || nextLower {
				b.WriteByte('_')
			}
		}
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r - 32)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// InboundRef captures a foreign key pointing AT some entity X — used to
// drive XInclude enum variant generation and the include slot fields on
// the target entity message.
type InboundRef struct {
	// FromEntityID is the entity that declares the FK (e.g. "consumer.Session").
	FromEntityID string
	// FromField is the column on FromEntityID holding the FK value.
	FromField string
	// FromEntity is a pointer back to the source entity so handlers can
	// reach its table name, PK type, and column list when emitting the
	// include attach helper. nil for cross-IR resolution failures.
	FromEntity *dsl.Entity
}

// InboundRefs scans every entity for `references` fields pointing AT each
// other entity, and returns a map keyed by target entity ID.
//
// The variant numbering of the XInclude enum is the slice index, so the sort
// is what keeps a number bound to the same reference across runs.
//
// One call before an emitter's main loop; cheap (O(entities × fields)).
//
// Shared by the codegen emitters and the runtime dispatcher's descriptor
// builder. A second copy is how the two come to disagree about which include
// variant carries which number, at which point a client's request selects a
// relation the server does not think it asked for.
func InboundRefs(ir *dsl.IR) map[string][]InboundRef {
	if ir == nil {
		return nil
	}
	out := map[string][]InboundRef{}
	for i := range ir.Entities {
		e := &ir.Entities[i]
		for _, f := range e.Fields {
			if f.Ref == nil || f.Ref.TargetID == "" {
				continue
			}
			out[f.Ref.TargetID] = append(out[f.Ref.TargetID], InboundRef{
				FromEntityID: e.ID(),
				FromField:    f.Name,
				FromEntity:   e,
			})
		}
	}
	for k := range out {
		sort.Slice(out[k], func(i, j int) bool {
			a, b := out[k][i], out[k][j]
			if a.FromEntityID != b.FromEntityID {
				return a.FromEntityID < b.FromEntityID
			}
			return a.FromField < b.FromField
		})
	}
	return out
}
