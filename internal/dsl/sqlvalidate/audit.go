package sqlvalidate

import (
	"fmt"

	pg "github.com/pganalyze/pg_query_go/v6"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// AuditForbiddenCalls reports every forbidden call in an IR that is already
// stored, across every surface caller SQL can occupy.
//
// # Why an audit, when there is already a gate
//
// The gate in this package runs at plan and apply. That makes it prospective
// only: SQL that entered the checkpoint before the gate existed has never been
// looked at, and it executes with exactly the same authority as SQL that has.
// `partition by` delegates tenant isolation to a run-time parameter precisely
// because a forgotten call site should not be able to leak — and a stored query
// body calling set_config is a forgotten call site with a head start.
//
// It also closes the deploy-order window. Migration 0024 makes the discriminator
// a PGC_USERSET parameter, and a server binary predating the gate would serve a
// checkpoint containing a call that the gate would now refuse. Nothing can stop
// an old binary from running, but a new one can decline to serve what the old
// one let in — which is where this is called from (cmd/server).
//
// # Why this is not the full validator
//
// Deliberately only the forbidden-call check. Re-running table resolution or
// touches coverage over a stored IR would resurrect the drift problem the admin
// service documents at its own call site: content that was legal when its owner
// applied it can stop resolving under later rules, and refusing to boot over
// that would turn one caller's staleness into an outage. A forbidden call is
// different in kind — it was never legal, and it is a live cross-tenant read.
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
