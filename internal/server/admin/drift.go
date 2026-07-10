package admin

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/introspect"
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

// checkDriftError is the structured refusal `tide apply` returns when the
// declared CHECK constraints and the live table's CHECK constraints diverge.
// It lists each divergence in both directions with the operator's remediation
// path and the override escape hatch. Mirrors indexDriftError's layout.
func checkDriftError(drift []introspect.CheckConstraintDrift) error {
	var b strings.Builder
	b.WriteString("apply blocked: CHECK constraints diverge between this schema and the live database.\n")
	b.WriteString("atlantis does not manage CHECK constraints on existing tables, so applying would leave the divergence in place.\n\n")
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
	b.WriteString("The diff path compares against the IR checkpoint, not the live DB, so applying would leave the divergence in place.\n\n")
	for _, d := range drift {
		fmt.Fprintf(&b, "  %s.%s.%s — declared %s, live %s\n", d.Schema, d.Table, d.Column, d.Declared, d.Live)
		fmt.Fprintf(&b, "    resolve: ALTER TABLE %s.%s ALTER COLUMN %s TYPE %s;\n", d.Schema, d.Table, d.Column, d.Declared)
	}
	b.WriteString("\nIf the live type is intentional, update your .atl to match — or set ATLANTIS_ALLOW_COLUMN_DRIFT=1 to apply anyway.")
	return errors.New(b.String())
}
