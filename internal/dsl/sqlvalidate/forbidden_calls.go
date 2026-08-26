package sqlvalidate

import (
	"fmt"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Functions caller-authored SQL may never call, and why.
//
// `partition by` rests on this gate. The tenant discriminator is a run-time
// parameter (migration 0024), and a custom GUC is PGC_USERSET —
// PostgreSQL offers no way to lock it. `REVOKE SET ON PARAMETER
// "atlantis.tenant" FROM PUBLIC` does not even create a pg_parameter_acl row
// for a placeholder GUC, verified on 17.8. checkStatementKind already refuses
// a bare `SET`, because it is a permit-list and VariableSetStmt is not on it.
// The function-call form was the hole:
//
//	SELECT ... FROM (SELECT set_config('atlantis.tenant','victim',true)) s,
//	                 atlantis.orders o
//
// is one SELECT statement, references a declared table, and passed every check
// this package ran. Executed against 17.8 it returns the victim's rows.
var forbiddenFunctions = map[string]string{
	"set_config": "set_config changes a run-time parameter for the session or " +
		"transaction. atlantis binds the caller's tenant to `atlantis.tenant` and " +
		"every `partition by` policy compares against it, so SQL that can call " +
		"set_config can read another tenant's rows. Bind the tenant through the " +
		"request, not through SQL",

	// The wrapper, not only the primitive.
	//
	// atlantis.set_partition is GRANTed to PUBLIC — it has to be, because the
	// server binds as the same role that runs caller SQL, so no privilege
	// boundary separates them. Blocking set_config while leaving the project's
	// own setter callable left the front door open: on a transaction nothing
	// has bound yet, `SELECT ... FROM (SELECT atlantis.set_partition('victim')) s,
	// orders o` binds the attacker's tenant and returns its rows, having passed
	// the statement gate, table resolution and touches. Executed on 17.8.
	//
	// In a correctly bound transaction the once-only guard turns this into an
	// error rather than a leak — but "correctly bound" is not yet true of any
	// path (task #38), so the unbound case is the live one.
	"set_partition": "atlantis.set_partition binds the caller's tenant for the " +
		"transaction, and every `partition by` policy compares against what it " +
		"sets. SQL that can call it can choose which tenant's rows the rest of " +
		"the transaction sees. atlantis binds the tenant from the request; SQL " +
		"must not",
}

// checkForbiddenCalls reports every call to a forbidden function anywhere in a
// parsed statement.
//
// Walks the protobuf message graph by reflection, visiting every field of every
// message, so an unrecognised node type is still descended into.
//
// A walk that names its node types is a permit-list of places to look. Postgres
// has over two hundred node types and pg_query adds more with each major
// version; an omission here is a cross-tenant read.
func checkForbiddenCalls(stmt *pg.Node, context string) []error {
	var errs []error
	seen := map[string]bool{}

	visit := func(m protoreflect.Message) {
		fc, ok := m.Interface().(*pg.FuncCall)
		if !ok || fc == nil {
			return
		}
		// The LAST element of funcname, not the qualified path.
		// `set_config(...)` and `pg_catalog.set_config(...)` are the same call,
		// so matching the qualified form only would be defeated by writing the
		// other. Postgres resolves an unqualified name through search_path, so
		// no prefix in the parse tree identifies the target reliably — which
		// makes the safe reading of `x.set_config(...)` that it might be
		// pg_catalog's. funcCallName (backfill.go) already takes the last
		// element; lowercase here because a quoted identifier keeps its case.
		name := strings.ToLower(funcCallName(fc))
		reason, forbidden := forbiddenFunctions[name]
		if !forbidden || seen[name] {
			return
		}
		seen[name] = true
		errs = append(errs, fmt.Errorf("%s: SQL calls %s(), which is not permitted here. %s",
			context, name, reason))
	}
	walkMessages(stmt.ProtoReflect(), visit)
	return errs
}

// walkMessages calls visit on m and on every message reachable from it,
// including through repeated fields and map values.
func walkMessages(m protoreflect.Message, visit func(protoreflect.Message)) {
	if m == nil || !m.IsValid() {
		return
	}
	visit(m)
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			if fd.MapValue().Kind() != protoreflect.MessageKind &&
				fd.MapValue().Kind() != protoreflect.GroupKind {
				return true
			}
			v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
				walkMessages(mv.Message(), visit)
				return true
			})
		case fd.IsList():
			if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
				return true
			}
			list := v.List()
			for i := 0; i < list.Len(); i++ {
				walkMessages(list.Get(i).Message(), visit)
			}
		case fd.Kind() == protoreflect.MessageKind, fd.Kind() == protoreflect.GroupKind:
			walkMessages(v.Message(), visit)
		}
		return true
	})
}

// Assert at compile time that the node type this file keys on is a proto
// message, so a pg_query_go upgrade that changes the representation fails to
// build rather than silently matching nothing.
var _ proto.Message = (*pg.FuncCall)(nil)
