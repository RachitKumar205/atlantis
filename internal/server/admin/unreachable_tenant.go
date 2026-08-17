package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// Turning on `partition by` over rows nobody will ever be able to bind to.
//
// # What makes a row unreachable
//
// The discriminator is a run-time parameter, and migration 0024 refuses to bind
// an empty one: `atlantis.set_partition` errors on it, and current_partition()
// NULLIFs it, so an unbound read compares against NULL and matches nothing. That
// is deliberate — it is what makes an unbound request fail closed.
//
// The empty string is nonetheless a legal value for the column: `not null` does not exclude
// it, and it is exactly what a legacy column gets when it is added and
// backfilled with a default before anyone decides what the tenants are. So the
// moment `partition by` is applied over that data, those rows stop being
// readable by anyone — no tenant can name them, and there is no error, because
// the policy is working exactly as written.
//
// # Why only when the clause is being ADDED
//
// An already-partitioned table cannot acquire such a row: the boundary's
// WITH CHECK compares the column to current_partition(), which is NULL there,
// so the insert is refused. A brand-new entity has no rows yet. The exposure is
// exactly the transition, over data that predates it.
//
// # Why a cap rather than an exact count
//
// The index on the discriminator is created by this same migration and does not
// exist while this runs, so an exact count is a sequential scan of a legacy
// table inside the apply's locked transaction. The operator needs to know that
// there ARE such rows and roughly how many, not the precise figure, so the scan
// stops at unreachableTenantCap.
const unreachableTenantCap = 1000

// unreachableTenant is one table about to isolate rows nobody can reach.
type unreachableTenant struct {
	EntityID string
	Schema   string
	Table    string
	Column   string
	Rows     int
	Capped   bool
}

// detectUnreachableTenantRows reports the tables gaining `partition by` in this
// migration that already hold rows with an empty discriminator.
func detectUnreachableTenantRows(
	ctx context.Context, tx pgx.Tx, d *codegen.Diff, newIR *dsl.IR,
) ([]unreachableTenant, error) {
	if d == nil || newIR == nil {
		return nil, nil
	}
	newByID := make(map[string]*dsl.Entity, len(newIR.Entities))
	for i := range newIR.Entities {
		newByID[newIR.Entities[i].ID()] = &newIR.Entities[i]
	}
	var out []unreachableTenant
	for _, ch := range d.All() {
		if ch.Kind != codegen.KindPartitionAdded {
			continue
		}
		e := newByID[ch.EntityID]
		if e == nil || e.PartitionField == "" {
			continue
		}
		qualified := schema.QuoteIdent(schema.EntitySchema(e)) + "." +
			schema.QuoteIdent(schema.EntityPhysicalTable(e))

		// The table may not exist yet — an entity created with the clause in
		// the same migration reaches here with nothing to scan.
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, qualified).Scan(&exists); err != nil {
			return nil, fmt.Errorf("check %s exists: %w", qualified, err)
		}
		if !exists {
			continue
		}

		var n int
		q := fmt.Sprintf(
			`SELECT count(*) FROM (SELECT 1 FROM %s WHERE %s = '' LIMIT %d) x`,
			qualified, schema.QuoteIdent(e.PartitionField), unreachableTenantCap+1)
		if err := tx.QueryRow(ctx, q).Scan(&n); err != nil {
			return nil, fmt.Errorf("count unreachable rows in %s: %w", qualified, err)
		}
		if n == 0 {
			continue
		}
		out = append(out, unreachableTenant{
			EntityID: ch.EntityID,
			Schema:   schema.EntitySchema(e),
			Table:    schema.EntityPhysicalTable(e),
			Column:   e.PartitionField,
			Rows:     min(n, unreachableTenantCap),
			Capped:   n > unreachableTenantCap,
		})
	}
	return out, nil
}

// unreachableTenantError is the structured refusal. Mirrors indexDriftError's
// layout: what is wrong, the remediation, then the override.
func unreachableTenantError(rows []unreachableTenant) error {
	var b strings.Builder
	b.WriteString("refusing to apply: tenant isolation would make existing rows unreachable.\n\n")
	b.WriteString("These tables are gaining `partition by` and already hold rows whose\n")
	b.WriteString("discriminator is the empty string. No caller can bind to '' — set_partition\n")
	b.WriteString("refuses it — so after this applies those rows are readable by nobody, and\n")
	b.WriteString("nothing will report it, because the policy is working as written.\n\n")
	for _, r := range rows {
		count := fmt.Sprintf("%d", r.Rows)
		if r.Capped {
			count = fmt.Sprintf("%d+", r.Rows)
		}
		fmt.Fprintf(&b, "  %s.%s — %s rows with %s = ''  (%s)\n",
			r.Schema, r.Table, count, r.Column, r.EntityID)
		fmt.Fprintf(&b, "    resolve: UPDATE %s.%s SET %s = '<tenant>' WHERE %s = '';\n",
			r.Schema, r.Table, r.Column, r.Column)
	}
	b.WriteString("\nIf those rows are meant to be retired rather than assigned, delete them first.\n")
	b.WriteString("If you accept losing access to them, set ATLANTIS_ALLOW_UNREACHABLE_TENANT=1 to apply anyway.")
	return errors.New(b.String())
}
