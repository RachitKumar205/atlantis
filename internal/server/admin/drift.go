package admin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/introspect"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// indexDriftError is the structured refusal `tide apply` returns when the
// live DB enforces a UNIQUE index the schema doesn't declare. It lists each
// drifting index with the exact remediation DDL (using the live index name,
// never a reconstructed one) and the override escape hatch. Mirrors
// extensionsMissingError's copy-paste-friendly layout.
func indexDriftError(drift []introspect.UniqueIndexDrift) error {
	var b strings.Builder
	b.WriteString("apply blocked: the live database enforces UNIQUE index(es) this schema does not declare.\n")
	b.WriteString("Applying would leave a hidden constraint that silently rejects legitimate writes.\n\n")
	for _, d := range drift {
		kind := "UNIQUE index"
		if d.Partial {
			kind = "partial UNIQUE index"
		}
		fmt.Fprintf(&b, "  %s.%s — %s on %s\n", d.Schema, d.Table, kind, d.Describe())
		fmt.Fprintf(&b, "    resolve: %s\n", d.DropStatement())
		if d.Partial {
			// The predicate text is the exact `pg_get_expr(indpred)` form; the
			// declared `where` must canonicalize-equal to it.
			fmt.Fprintf(&b, "    or declare it in your .atl: `unique index partial by %s where %s`\n",
				strings.Join(d.Columns, ", "), d.Predicate)
		} else {
			fmt.Fprintf(&b, "    or declare the uniqueness in your .atl (field `unique`, or `unique by %s`)\n", strings.Join(d.Columns, ", "))
		}
	}
	b.WriteString("\nIf this index is intentional and you accept it, set ATLANTIS_ALLOW_INDEX_DRIFT=1 to apply anyway.")
	return errors.New(b.String())
}

// withAddedEntities returns prior with the entities of newIR that prior lacks
// appended, or newIR when prior is nil.
func withAddedEntities(prior, newIR *dsl.IR) *dsl.IR {
	if prior == nil {
		return newIR
	}
	have := make(map[string]bool, len(prior.Entities))
	for i := range prior.Entities {
		have[prior.Entities[i].ID()] = true
	}
	out := &dsl.IR{Entities: append([]dsl.Entity(nil), prior.Entities...)}
	for i := range newIR.Entities {
		if !have[newIR.Entities[i].ID()] {
			out.Entities = append(out.Entities, newIR.Entities[i])
		}
	}
	return out
}

// driftAllowed reports whether the named ATLANTIS_ALLOW_*_DRIFT variable is
// set to accept that drift.
func driftAllowed(name string) bool { return os.Getenv(name) == "1" }

// renderForDrift renders newIR for refuseUnresolvedDrift, or returns nil when
// every drift check is overridden.
func renderForDrift(ctx context.Context, q introspect.DBTX, newIR *dsl.IR) (*introspect.Declared, error) {
	if driftAllowed("ATLANTIS_ALLOW_INDEX_DRIFT") && driftAllowed("ATLANTIS_ALLOW_CHECK_DRIFT") &&
		driftAllowed("ATLANTIS_ALLOW_COLUMN_DRIFT") {
		return nil, nil
	}
	declared, err := introspect.RenderDeclared(ctx, q, newIR)
	if err != nil {
		return nil, fmt.Errorf("apply: render the schema for drift checks: %w", err)
	}
	return declared, nil
}

// refuseUnresolvedDrift returns an error when the database q reads has a bare
// UNIQUE index, a CHECK constraint or a column type that differs from
// declared. ATLANTIS_ALLOW_INDEX_DRIFT=1, ATLANTIS_ALLOW_CHECK_DRIFT=1 and
// ATLANTIS_ALLOW_COLUMN_DRIFT=1 skip the respective check.
//
// The caller must run it after the migration's DDL, in the same transaction.
func refuseUnresolvedDrift(ctx context.Context, q introspect.DBTX, declared *introspect.Declared) error {
	if declared == nil {
		return nil
	}
	declared.Settle(ctx, q)
	// The diff compares against the checkpoint, so an object that diverged
	// outside atlantis stays diverged through an apply: a hidden UNIQUE
	// rejecting legitimate writes, the carts `awaiting_checkout` outage, and
	// varchar(10) live under a varchar(255) declaration.
	if !driftAllowed("ATLANTIS_ALLOW_INDEX_DRIFT") {
		drift, _, err := declared.UniqueIndexDrift(ctx, q)
		if err != nil {
			return fmt.Errorf("apply: index-drift check failed: %w", err)
		}
		if len(drift) > 0 {
			return indexDriftError(drift)
		}
	}
	if !driftAllowed("ATLANTIS_ALLOW_CHECK_DRIFT") {
		drift, _, err := declared.CheckDrift(ctx, q)
		if err == nil {
			drift, err = withoutParkedChecks(ctx, q, drift)
		}
		if err != nil {
			return fmt.Errorf("apply: check-drift check failed: %w", err)
		}
		if len(drift) > 0 {
			return checkDriftError(drift)
		}
	}
	if !driftAllowed("ATLANTIS_ALLOW_COLUMN_DRIFT") {
		drift, _, err := declared.ColumnDrift(ctx, q)
		if err != nil {
			return fmt.Errorf("apply: column-drift check failed: %w", err)
		}
		if len(drift) > 0 {
			return columnDriftError(drift)
		}
	}
	return nil
}

// refuseStaleTypeChanges returns an error when a column whose type d changes
// has a live type matching neither prior nor newIR. The caller must run it
// before the migration's DDL.
//
// ATLANTIS_ALLOW_COLUMN_DRIFT=1 skips it.
func refuseStaleTypeChanges(ctx context.Context, q introspect.Querier, prior, newIR *dsl.IR, d *codegen.Diff) error {
	if prior == nil || driftAllowed("ATLANTIS_ALLOW_COLUMN_DRIFT") {
		return nil
	}
	type column struct {
		sch, tbl, name string
		before, after  string // the prior and new types as the migration spells them
		live           liveType
		liveText       string
		found          bool
	}
	byID := func(ir *dsl.IR) map[string]*dsl.Entity {
		out := make(map[string]*dsl.Entity, len(ir.Entities))
		for i := range ir.Entities {
			out[ir.Entities[i].ID()] = &ir.Entities[i]
		}
		return out
	}
	priorByID, newByID := byID(prior), byID(newIR)
	var cols []column
	for _, ch := range d.All() {
		if ch.Kind != codegen.KindFieldTypeChanged {
			continue
		}
		pe, ne := priorByID[ch.EntityID], newByID[ch.EntityID]
		if pe == nil || ne == nil || pe.FindField(ch.Field) == nil || ne.FindField(ch.Field) == nil {
			continue // a restored field's type is checked by diffWithParked
		}
		sch, tbl := codegen.PhysicalTable(ne)
		cols = append(cols, column{sch: sch, tbl: tbl, name: ch.Field,
			before: schema.SQLType(pe.FindField(ch.Field).Type),
			after:  schema.SQLType(ne.FindField(ch.Field).Type)})
	}
	if len(cols) == 0 {
		return nil
	}

	schemas, tables, names := make([]string, len(cols)), make([]string, len(cols)), make([]string, len(cols))
	for i, c := range cols {
		schemas[i], tables[i], names[i] = c.sch, c.tbl, c.name
	}
	rows, err := q.Query(ctx, `
SELECT t.i, a.atttypid, a.atttypmod, format_type(a.atttypid, a.atttypmod)
  FROM unnest($1::text[], $2::text[], $3::text[]) WITH ORDINALITY AS t(sch, tbl, col, i)
  JOIN pg_attribute a
    ON a.attrelid = to_regclass(format('%I.%I', t.sch, t.tbl))
   AND a.attname = t.col AND a.attnum > 0 AND NOT a.attisdropped`, schemas, tables, names)
	if err != nil {
		return fmt.Errorf("apply: column-drift check failed: %w", err)
	}
	for rows.Next() {
		var i int64
		var c column
		if err := rows.Scan(&i, &c.live.oid, &c.live.mod, &c.liveText); err != nil {
			rows.Close()
			return fmt.Errorf("apply: column-drift check failed: %w", err)
		}
		cols[i-1].live, cols[i-1].liveText, cols[i-1].found = c.live, c.liveText, true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("apply: column-drift check failed: %w", err)
	}

	// The ALTER TYPE converts from whatever the column holds: from the type
	// it was planned against that is the reviewed change, from another it
	// can drop precision, as numeric(12,4) to numeric(12,2) does. A new type
	// that does not resolve yet is not the live one.
	var found []int
	var live []liveType
	var before, after []string
	for i, c := range cols {
		if c.found {
			found = append(found, i)
			live, before, after = append(live, c.live), append(before, c.before), append(after, c.after)
		}
	}
	matches, err := sameTypes(ctx, q, live, before)
	if err != nil {
		return fmt.Errorf("apply: column-drift check failed: %w", err)
	}
	becomes, err := sameTypes(ctx, q, live, after)
	if err != nil {
		return fmt.Errorf("apply: column-drift check failed: %w", err)
	}

	var b strings.Builder
	for k, i := range found {
		if matches[k] || becomes[k] {
			continue
		}
		c := cols[i]
		fmt.Fprintf(&b, "  %s.%s.%s — last applied %s, live %s\n", c.sch, c.tbl, c.name, c.before, c.liveText)
		fmt.Fprintf(&b, "    resolve: ALTER TABLE %s.%s ALTER COLUMN %s TYPE %s; or declare %s and re-plan\n",
			c.sch, c.tbl, c.name, c.before, c.liveText)
	}
	if b.Len() == 0 {
		return nil
	}
	return errors.New("apply blocked: this plan changes the type of a column whose live type differs from the last applied schema.\n" +
		"The change was planned from the last applied type, and converting from the live one can lose data.\n\n" +
		b.String() + "\nIf the conversion is safe, set ATLANTIS_ALLOW_COLUMN_DRIFT=1 to apply anyway.")
}

// refuseMissingTables returns an error when a table newIR declares does not
// exist. The caller must run it after the migration's DDL, which creates the
// table of every entity the plan adds.
func refuseMissingTables(ctx context.Context, q introspect.Querier, newIR *dsl.IR) error {
	names := make([]string, 0, len(newIR.Entities))
	for i := range newIR.Entities {
		sch, tbl := codegen.PhysicalTable(&newIR.Entities[i])
		names = append(names, pgx.Identifier{sch, tbl}.Sanitize())
	}
	rows, err := q.Query(ctx, `SELECT t FROM unnest($1::text[]) AS t WHERE to_regclass(t) IS NULL ORDER BY t`, names)
	if err != nil {
		return fmt.Errorf("apply: table check failed: %w", err)
	}
	var b strings.Builder
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return fmt.Errorf("apply: table check failed: %w", err)
		}
		fmt.Fprintf(&b, "  %s\n", t)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("apply: table check failed: %w", err)
	}
	if b.Len() == 0 {
		return nil
	}
	return errors.New("apply blocked: tables this schema declares do not exist after the migration:\n" +
		b.String() + "\nA changed `table` name is not applied by atlantis: rename the table, then apply again.")
}

// withoutParkedChecks drops the live-but-undeclared entries for CHECK
// constraints on parked columns alone, which keep their constraints until
// reaped or restored. It reads the database only when there is such an entry.
func withoutParkedChecks(ctx context.Context, q introspect.Querier, drift []introspect.CheckConstraintDrift) ([]introspect.CheckConstraintDrift, error) {
	var schemas, tables []string
	for _, d := range drift {
		if d.Kind == introspect.CheckLiveNotDeclared {
			schemas, tables = append(schemas, d.Schema), append(tables, d.Table)
		}
	}
	if len(schemas) == 0 {
		return drift, nil
	}
	parkedSQL, err := parkedAttrsSQL(ctx, q, true)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
WITH parked AS (`+parkedSQL+`),
     per_table AS (SELECT attrelid, array_agg(attnum) AS nums FROM parked GROUP BY attrelid)
SELECT n.nspname, c.relname, con.conname
  FROM pg_constraint con
  JOIN per_table pt ON pt.attrelid = con.conrelid
  JOIN pg_class c ON c.oid = con.conrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE con.contype = 'c' AND con.conkey <@ pt.nums`, schemas, tables)
	if err != nil {
		return nil, err
	}
	parked := map[[3]string]bool{}
	for rows.Next() {
		var k [3]string
		if err := rows.Scan(&k[0], &k[1], &k[2]); err != nil {
			rows.Close()
			return nil, err
		}
		parked[k] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := drift[:0:0]
	for _, d := range drift {
		if d.Kind == introspect.CheckLiveNotDeclared && parked[[3]string{d.Schema, d.Table, d.ConstraintName}] {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// checkDriftError is the structured refusal `tide apply` returns when the
// declared CHECK constraints and the live table's CHECK constraints diverge.
// It lists each divergence in both directions with the operator's remediation
// path and the override escape hatch. Mirrors indexDriftError's layout.
func checkDriftError(drift []introspect.CheckConstraintDrift) error {
	var b strings.Builder
	b.WriteString("apply blocked: CHECK constraints diverge between this schema and the live database.\n")
	b.WriteString("The live constraints still differ from the schema after this migration, so nothing was applied.\n\n")
	for _, d := range drift {
		switch d.Kind {
		case introspect.CheckDeclaredNotEnforced:
			fmt.Fprintf(&b, "  %s.%s — declared check is NOT enforced live:\n", d.Schema, d.Table)
			fmt.Fprintf(&b, "      declared: %s\n", d.Declared)
			fmt.Fprintf(&b, "      resolve:  reconcile the live constraint to %s (drop the stale one, then ADD CONSTRAINT)\n", d.Definition)
		case introspect.CheckLiveNotDeclared:
			fmt.Fprintf(&b, "  %s.%s — live constraint %q is NOT declared:\n", d.Schema, d.Table, d.ConstraintName)
			fmt.Fprintf(&b, "      live:    %s\n", d.Definition)
			fmt.Fprintf(&b, "      resolve: declare it in your .atl, or DROP CONSTRAINT %s if unintended\n", d.ConstraintName)
		}
	}
	b.WriteString("\nIf the difference is intentional or cosmetic (e.g. `col IS NULL OR ...`), set ATLANTIS_ALLOW_CHECK_DRIFT=1 to apply anyway.")
	return errors.New(b.String())
}

// columnDriftError is the structured refusal `tide apply` returns when a
// column's live type/width diverges from the declaration. Mirrors
// indexDriftError's layout.
func columnDriftError(drift []introspect.ColumnTypeDrift) error {
	var b strings.Builder
	b.WriteString("apply blocked: column type(s) diverge between this schema and the live database.\n")
	b.WriteString("The live columns still differ from the schema after this migration, so nothing was applied.\n\n")
	for _, d := range drift {
		fmt.Fprintf(&b, "  %s.%s.%s — declared %s, live %s\n", d.Schema, d.Table, d.Column, d.Declared, d.Live)
		fmt.Fprintf(&b, "    resolve: ALTER TABLE %s.%s ALTER COLUMN %s TYPE %s;\n", d.Schema, d.Table, d.Column, d.Declared)
	}
	b.WriteString("\nIf the live type is intentional, update your .atl to match — or set ATLANTIS_ALLOW_COLUMN_DRIFT=1 to apply anyway.")
	return errors.New(b.String())
}
