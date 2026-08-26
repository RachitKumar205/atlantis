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

// ValidateBackfillExpression parses the SQL behind a field's
// `backfill "<expr>"` modifier and rejects it if any of:
//
//   - it does not parse as the right-hand side of `SET <col> = <expr>`
//   - it contains a subquery (SubLink), which reads arbitrary tables
//   - it calls a function outside safeBackfillFunctions, which admits volatile
//     and side-effecting ones to the apply path
//   - it references its own field, selfFieldName
//   - it references a column absent from allowedCols
//
// allowedCols is the column set of the entity carrying the field, minus the
// columns being added in the same plan; those hold no values when the backfill
// runs.
func ValidateBackfillExpression(expr string, allowedCols map[string]bool, selfFieldName string) error {
	if strings.TrimSpace(expr) == "" {
		return errors.New("backfill expression is empty")
	}
	// Parsed inside the statement the emitter builds. internal/backfill's
	// ChunkSQL writes `SET %[3]s = %[4]s` with no wrapper, so a comma in the
	// expression starts a new assignment:
	//
	//	'n', tenant = 'attacker', secret = 'clobbered'
	//
	// A `SELECT (<expr>)` probe accepts that, the parentheses making it a row
	// constructor, and it rewrites the tenant discriminator of every row at
	// apply time.
	//
	// The probe table and column are placeholders; parsing consults no catalog.
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
	// opened others. A `SELECT (<expr>)` probe hides this: the parentheses turn
	// the comma list into a row constructor.
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
	// A permit-list of node types with no case for MinMaxExpr, A_Indirection,
	// CollateClause or BooleanTest accepts every one of these:
	//
	//	GREATEST('a', (SELECT ir::text FROM atlantis.ir_checkpoint LIMIT 1))
	//	((SELECT ir::text FROM atlantis.ir_checkpoint LIMIT 1))[1]
	//	(SELECT ir::text FROM atlantis.ir_checkpoint LIMIT 1) COLLATE "C"
	//	(SELECT true) IS TRUE
	//
	// spliced into a live UPDATE: read any table, write it into a column. A
	// reflection pass has nothing to keep in sync as pg_query's node set grows.
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
