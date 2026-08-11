package sqlvalidate

import (
	"errors"
	"fmt"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// safeBackfillFunctions is the closed set of function names a backfill
// expression may call. Anything outside this set is rejected because the
// expression runs server-side at apply time with the atlantis server's
// PG privileges — `nextval()` would mutate sequence state, `current_setting()`
// would leak runtime config, `pg_read_file()` would read arbitrary files
// from the data directory. The whitelist is intentionally tiny; operators
// who need more lobby for additions one function at a time.
var safeBackfillFunctions = map[string]bool{
	"coalesce":    true,
	"nullif":      true,
	"lower":       true,
	"upper":       true,
	"length":      true,
	"char_length": true,
	"trim":        true,
	"ltrim":       true,
	"rtrim":       true,
	"abs":         true,
	"concat":      true,
	"concat_ws":   true,
	"substring":   true,
	"substr":      true,
	"replace":     true,
	"left":        true,
	"right":       true,
	"to_char":     true,
}

// ValidateBackfillExpression parses the user-supplied SQL expression
// behind a field's `backfill "<expr>"` modifier and rejects it if any of:
//
//   - it does not parse as a valid scalar SQL expression
//   - it contains a subquery (SubLink) — would let the expression read
//     arbitrary tables, including ones the operator doesn't intend
//   - it calls a function outside safeBackfillFunctions — would let
//     volatile / side-effect-producing functions slip into the apply path
//   - it references its own field (selfFieldName) — pointless and
//     misleading
//   - it references a column not present in allowedCols — typos or
//     stale references that would silently break at apply time
//
// allowedCols should be the column set of the entity carrying the field,
// minus any columns being added in the same plan (those aren't populated
// yet when the backfill runs).
//
// The parser sees `SELECT (<expr>)` so the expression position matches
// its real apply-time context (the RHS of `SET <col> = <expr>` in the
// chunked UPDATE). Wrapping in SELECT not UPDATE keeps the validator
// independent of schema layout — it doesn't need a real table to parse.
func ValidateBackfillExpression(expr string, allowedCols map[string]bool, selfFieldName string) error {
	if strings.TrimSpace(expr) == "" {
		return errors.New("backfill expression is empty")
	}
	// Parsed in the shape it is SPLICED into, not a shape that resembles it.
	//
	// This was `SELECT (%s)`, and the comment above claimed that matched "the
	// RHS of SET <col> = <expr>". It does not: the parentheses are not in the
	// SET list, and they are what makes the difference. `internal/backfill`
	// writes `SET %[3]s = %[4]s` with no wrapper, so a comma in the expression
	// starts a NEW ASSIGNMENT, and the probe swallowed it inside the parens as
	// a row constructor. Executed against a live table:
	//
	//	'n', tenant = 'attacker', secret = 'clobbered'
	//
	// passed this gate, and rewrote the tenant discriminator of every row in
	// the table at apply time. A backfill that reads as "populate the new
	// nickname column" moved rows between tenants.
	//
	// The same defect was found and fixed twice before this — in the CHECK
	// gate, which probed `SELECT (expr)` while codegen emitted `CONSTRAINT c
	// CHECK (expr)`, and in the index-expression gate. It was not carried here.
	// The rule is now general: a validator MUST parse the expression inside the
	// exact statement the emitter builds, or it is testing a different grammar.
	//
	// The probe table and column are placeholders; nothing resolves them,
	// because parsing does not consult a catalog.
	src := fmt.Sprintf("UPDATE atlantis_backfill_probe t SET atlantis_probe_col = %s", expr)
	tree, err := pg.Parse(src)
	if err != nil {
		return fmt.Errorf("backfill expression parse failed: %w", err)
	}
	if len(tree.Stmts) != 1 {
		return fmt.Errorf("backfill expression must be a single scalar; got %d statements", len(tree.Stmts))
	}
	upd := tree.Stmts[0].GetStmt().GetUpdateStmt()
	if upd == nil {
		return errors.New("backfill expression does not parse as the right-hand " +
			"side of an assignment. It is spliced into `SET <column> = <expr>`")
	}
	// Exactly one assignment, or the expression closed the one it was given and
	// opened others. This is the check the parentheses used to hide.
	if n := len(upd.TargetList); n != 1 {
		return fmt.Errorf("backfill expression sets %d columns; it may only "+
			"produce a value for its own. An expression containing a comma at "+
			"assignment level ends its own assignment and begins another, and "+
			"the extra assignments are spliced into a live UPDATE", n)
	}
	// And no clauses the template does not expect: `WHERE`, `FROM` and
	// `RETURNING` are all reachable from the RHS position and all change which
	// rows the real statement touches.
	if upd.WhereClause != nil || len(upd.FromClause) > 0 || len(upd.ReturningList) > 0 {
		return errors.New("backfill expression carries a WHERE, FROM or RETURNING " +
			"clause. The expression is spliced into a chunked UPDATE that supplies " +
			"its own, and an added clause changes which rows are written")
	}

	exprNode := upd.TargetList[0].GetResTarget().GetVal()
	if exprNode == nil {
		return errors.New("backfill expression is empty")
	}

	var errs []string
	// Before the column and subquery pass below: an exhaustive reflection pass over the
	// whole parse tree, so nothing depends on that walk having a case for the
	// node a call happens to sit under.
	//
	// The walk below is a permit-list of node types, and this package has now
	// been bitten twice by exactly that shape — collectTableRefs missed a table
	// in 14 of 16 ordinary SQL forms, and the forbidden-call gate missed a
	// CHECK expression entirely. A backfill expression is spliced verbatim into
	// a live `UPDATE ... SET <col> = <expr>` by internal/backfill/splicer.go,
	// so a call the walk cannot see is a call that runs against real rows.
	//
	// Two passes rather than one: this one answers "is any call forbidden",
	// the next answers "does this reference anything it may not". Both are now
	// exhaustive; they stayed separate only because they report differently.
	{
		walkMessages(exprNode.ProtoReflect(), func(m protoreflect.Message) {
			fc, ok := m.Interface().(*pg.FuncCall)
			if !ok || fc == nil {
				return
			}
			name := strings.ToLower(funcCallName(fc))
			if reason, forbidden := forbiddenFunctions[name]; forbidden {
				errs = append(errs, fmt.Sprintf(
					"backfill expression calls %s(), which is not permitted. %s",
					name, reason))
			} else if !safeBackfillFunctions[name] {
				errs = append(errs, fmt.Sprintf(
					"backfill expression cannot call %q (not in the safe-function "+
						"list). It is spliced verbatim into an UPDATE against live "+
						"rows", name))
			}
		})
	}

	// Subqueries and column references, found the same exhaustive way.
	//
	// These two were a hand-written permit-list of node types while the function
	// check three lines above was reflective — in the SAME function, with a
	// comment explaining exactly why a permit-list is inadequate. The list had
	// no case for MinMaxExpr, A_Indirection, CollateClause or BooleanTest, so
	// every one of these was accepted:
	//
	//	GREATEST('a', (SELECT ir::text FROM atlantis.ir_checkpoint LIMIT 1))
	//	((SELECT ir::text FROM atlantis.ir_checkpoint LIMIT 1))[1]
	//	(SELECT ir::text FROM atlantis.ir_checkpoint LIMIT 1) COLLATE "C"
	//	(SELECT true) IS TRUE
	//
	// spliced into a live UPDATE — read any table, write it into a column.
	//
	// Adding the four missing cases would have been the third time this package
	// patched a walk instead of replacing it. A reflection pass cannot have a
	// missing case, so there is nothing left to keep in sync as pg_query's node
	// set grows.
	walkMessages(exprNode.ProtoReflect(), func(m protoreflect.Message) {
		switch node := m.Interface().(type) {
		case *pg.SubLink:
			if node != nil {
				errs = append(errs, "backfill expression cannot contain subqueries")
			}
		case *pg.ColumnRef:
			name := columnRefName(node)
			if name == "" {
				return
			}
			if selfFieldName != "" && name == selfFieldName {
				errs = append(errs, fmt.Sprintf(
					"backfill expression cannot reference its own column %q", name))
				return
			}
			if !allowedCols[name] {
				errs = append(errs, fmt.Sprintf(
					"backfill expression references unknown column %q", name))
			}
		}
	})
	// One subquery diagnostic, however many SubLinks are nested.
	errs = dedupeStrings(errs)
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func funcCallName(fc *pg.FuncCall) string {
	if fc == nil || len(fc.Funcname) == 0 {
		return ""
	}
	last := fc.Funcname[len(fc.Funcname)-1]
	if s := last.GetString_(); s != nil {
		return s.GetSval()
	}
	return ""
}

func columnRefName(cr *pg.ColumnRef) string {
	if cr == nil || len(cr.Fields) == 0 {
		return ""
	}
	last := cr.Fields[len(cr.Fields)-1]
	if s := last.GetString_(); s != nil {
		return s.GetSval()
	}
	return ""
}

// dedupeStrings keeps the first occurrence of each message.
//
// The subquery diagnostic is emitted per SubLink node, and the reflection pass
// reaches nested ones the old walk returned early from. Reporting the same
// sentence four times for one subquery reads as four problems.
func dedupeStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
