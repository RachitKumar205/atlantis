package entity

import (
	"fmt"
	"sort"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/dsl/sqlparams"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// entitySnapshot is an immutable point-in-time view of all entity and
// custom-query metadata derived from one IR. Swapped atomically on
// hot-reload via Server.snapshot; handlers load the current snapshot at
// each request. The old snapshot stays alive until all in-flight
// requests holding a reference complete.
type entitySnapshot struct {
	entities    map[string]*entityMeta
	customMeta  map[string]*customQueryMeta
	procMeta    map[string]*customProcMeta
	contentHash string
}

// buildSnapshot constructs a complete snapshot from an IR. Pure
// function — no side effects, no gRPC registration.
func buildSnapshot(ir *dsl.IR, contentHash string) (*entitySnapshot, error) {
	snap := &entitySnapshot{
		entities:    make(map[string]*entityMeta, len(ir.Entities)),
		customMeta:  make(map[string]*customQueryMeta, len(ir.Queries)),
		procMeta:    make(map[string]*customProcMeta, len(ir.Procedures)),
		contentHash: contentHash,
	}

	for i := range ir.Entities {
		e := &ir.Entities[i]
		meta := buildEntityMeta(e, ir)

		fd, err := buildProtoDescriptors(e)
		if err != nil {
			return nil, fmt.Errorf("entity %s: %w", e.ID(), err)
		}
		resolveProtoDescriptors(meta, fd)

		if meta.msgDesc == nil {
			return nil, fmt.Errorf("entity %s: entity message descriptor not built", e.ID())
		}
		if meta.getRequestDesc == nil {
			return nil, fmt.Errorf("entity %s: GetRequest descriptor not built", e.ID())
		}

		snap.entities[e.ID()] = meta
	}

	for i := range ir.Queries {
		cq := &ir.Queries[i]
		parts := splitEntityID(cq.Owner)
		ns := parts[0]

		// Rewrite `$name` → `$N` and capture the per-name arg order so
		// the dispatcher can bind values in the order PG expects.
		// Without this, raw `$user_id` is sent to PG verbatim and PG
		// errors with `syntax error at or near "$"`. Codegen does the
		// same rewrite via the same package for the client side.
		normSQL, argOrder, err := sqlparams.NormalizeNamed(cq.SQL, cq.Inputs)
		if err != nil {
			return nil, fmt.Errorf("custom query %s: %w", cq.Name, err)
		}

		cqm := &customQueryMeta{
			query:     cq,
			sql:       normSQL,
			inputCols: cq.Inputs,
			argOrder:  argOrder,
			timeoutMS: 2000,
		}

		if cq.Output.AsEntityID != "" {
			cqm.asEntity = true
			if em, ok := snap.entities[cq.Output.AsEntityID]; ok {
				cqm.entityMeta = em
			}
		} else {
			cqm.outputCols = cq.Output.Columns
		}

		fd, err := buildCustomQueryDescs(cq, ns)
		if err != nil {
			return nil, fmt.Errorf("custom query %s: %w", cq.Name, err)
		}

		cqm.requestDesc = fd.Messages().ByName(protoreflect.Name(cq.Name + "Request"))
		cqm.responseDesc = fd.Messages().ByName(protoreflect.Name(cq.Name + "Response"))

		if len(cq.Output.Columns) > 0 && cqm.responseDesc != nil {
			rowName := protoreflect.Name(cq.Name + "Response_Row")
			cqm.rowDesc = cqm.responseDesc.Messages().ByName(rowName)
		}

		key := ns + ":" + cq.Name
		snap.customMeta[key] = cqm
	}

	for i := range ir.Procedures {
		cp := &ir.Procedures[i]
		parts := splitEntityID(cp.Owner)
		ns := parts[0]

		pm := &customProcMeta{
			proc:      cp,
			inputCols: cp.Inputs,
			timeoutMS: customProcTimeoutMS,
		}

		// Each step is normalized independently: `$name` → `$N` where N
		// is the first-reference order WITHIN that step's SQL (the
		// ordinal map only validates that names are declared). So each
		// step carries its own argOrder — never accumulated across steps.
		touched := make(map[string]struct{})
		for si := range cp.Steps {
			step := &cp.Steps[si]
			switch {
			case step.Raw != nil:
				normSQL, argOrder, err := sqlparams.NormalizeNamed(step.Raw.SQL, cp.Inputs)
				if err != nil {
					return nil, fmt.Errorf("procedure %s step %d: %w", cp.Name, si, err)
				}
				pm.steps = append(pm.steps, procStep{sql: normSQL, argOrder: argOrder})
				for _, t := range step.Raw.Touches {
					touched[t] = struct{}{}
				}
			case step.Typed != nil:
				// Typed-verb steps (update/delete/insert) render SQL from
				// entity metadata; not yet supported by the dynamic
				// executor. Registered so the method exists, but the
				// handler returns a clear Unimplemented.
				pm.unsupported = "typed-verb step"
			case step.Enqueue != nil:
				pm.unsupported = "enqueue step"
			}
		}
		pm.touched = sortedKeys(touched)

		fd, err := buildCustomProcedureDescs(cp, ns)
		if err != nil {
			return nil, fmt.Errorf("procedure %s: %w", cp.Name, err)
		}
		pm.requestDesc = fd.Messages().ByName(protoreflect.Name(cp.Name + "Request"))
		pm.responseDesc = fd.Messages().ByName(protoreflect.Name(cp.Name + "Response"))

		key := ns + ":" + cp.Name
		snap.procMeta[key] = pm
	}

	return snap, nil
}

// sortedKeys returns the map keys in sorted order — used so the touched
// -entity invalidation order is deterministic.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
