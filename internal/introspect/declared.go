package introspect

import (
	"context"
	"fmt"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Declared is a schema's CHECK constraints, column types and partial unique
// predicates in the form Postgres stores them. Rendering creates and rolls
// back temporary tables and reads no user table; the comparisons read the
// live catalog.
type Declared struct {
	checks  *renderedChecks
	columns *renderedColumns
	uniques *renderedUniques
}

// Settle renders again what RenderDeclared could not, such as a column of a
// type the migration creates. The caller must call it once, before the
// comparisons.
func (d *Declared) Settle(ctx context.Context, q DBTX) {
	d.checks.settle(ctx, q, true)
	d.columns.settle(ctx, q)
	d.uniques.settle(ctx, q)
}

// RenderDeclared renders ir through the database q reads.
func RenderDeclared(ctx context.Context, q DBTX, ir *dsl.IR) (*Declared, error) {
	if ir == nil {
		return nil, fmt.Errorf("introspect: declaredIR is required")
	}
	return &Declared{
		checks:  renderChecks(ctx, q, ir),
		columns: renderColumns(ctx, q, ir),
		uniques: renderUniques(ctx, q, ir),
	}, nil
}

// CheckDrift is DetectCheckConstraintDrift against the rendered schema.
func (d *Declared) CheckDrift(ctx context.Context, q Querier) ([]CheckConstraintDrift, []string, error) {
	drift, err := d.checks.drift(ctx, q)
	return drift, d.checks.notes, err
}

// ColumnDrift is DetectColumnTypeDrift against the rendered schema.
func (d *Declared) ColumnDrift(ctx context.Context, q Querier) ([]ColumnTypeDrift, []string, error) {
	return d.columns.drift(ctx, q)
}

// UniqueIndexDrift is DetectUniqueIndexDrift against the rendered schema.
func (d *Declared) UniqueIndexDrift(ctx context.Context, q Querier) ([]UniqueIndexDrift, []string, error) {
	return d.uniques.drift(ctx, q)
}
