package sqlvalidate

import (
	"fmt"

	pg "github.com/pganalyze/pg_query_go/v6"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// AuditForbiddenCalls reports every forbidden call in an IR that is already
// stored, across every surface caller SQL can occupy.
//
// The gate in this package runs at plan and apply, so it is prospective: SQL
// that entered the checkpoint before the gate existed executes with the same
// authority as SQL that passed it. cmd/server calls this at boot, so a new
// binary declines to serve what an older one admitted.
//
// The forbidden-call check only, not the full validator. Table resolution and
// touches coverage re-run over a stored IR refuse to boot on content that was
// legal when applied and stopped resolving under a later rule. A forbidden call
// was never legal.
func AuditForbiddenCalls(ir *dsl.IR) []error {
	if ir == nil {
		return nil
	}
	var errs []error

	for i := range ir.Entities {
		if err := ValidateEntityExpressions(&ir.Entities[i]); err != nil {
			errs = append(errs, err)
		}
	}
	for i := range ir.Queries {
		errs = append(errs, auditBody(ir.Queries[i].SQL,
			fmt.Sprintf("stored query %s", ir.Queries[i].ID()))...)
	}
	for i := range ir.Procedures {
		p := &ir.Procedures[i]
		for j := range p.Steps {
			if p.Steps[j].Raw == nil {
				continue
			}
			errs = append(errs, auditBody(p.Steps[j].Raw.SQL,
				fmt.Sprintf("stored procedure %s step %d", p.ID(), j+1))...)
		}
	}
	return errs
}

// auditBody applies the forbidden-call gate to one stored SQL body.
//
// A body that no longer parses is skipped rather than reported. It cannot be
// executed either, so it is not a security finding, and reporting it would make
// this refuse to boot over a syntax problem that belongs to whoever applied it.
func auditBody(sql, context string) []error {
	if sql == "" {
		return nil
	}
	tree, err := pg.Parse(normalizeNamedParams(sql))
	if err != nil {
		return nil
	}
	var errs []error
	for _, raw := range tree.Stmts {
		errs = append(errs, checkForbiddenCalls(raw.GetStmt(), context)...)
	}
	return errs
}
