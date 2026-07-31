package entity

import (
	"context"
	"sort"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// Cross-entity cache invalidation, declared by the `invalidate_on` clause.
//
// The rule is written on the PARENT and names the CHILD:
//
//	entity Cart in consumer {
//	  cache { invalidate_on: write(self), write(CartItem where cart_id = self.id) }
//	}
//
// meaning a write to CartItem must invalidate the Cart whose id equals that
// item's cart_id. `write(self)` needs nothing here — handleCreate/Update/Delete
// already enqueue a self-invalidation on every write, unconditionally — so this
// file is only concerned with the cross-entity half.
//
// The index is inverted relative to the declaration. Declarations are
// parent-centric, but the lookup happens on the write path of the CHILD, which
// knows only its own entity id. So rules are keyed by child.
type inboundRule struct {
	// parentEntityID is the entity to invalidate — the one that declared the rule.
	parentEntityID string
	// childCol is the column on the written (child) row holding the parent's key.
	childCol string
	// parentCol is the column on the parent that childCol points at. Retained
	// for validation and diagnostics; the cache id is built from the value, and
	// the parent's own key order is what makes it meaningful.
	parentCol string
}

// buildInboundIndex maps a child entity id to the rules that fire when it is
// written.
//
// Rules naming an entity that does not exist are dropped rather than carried:
// a rule that cannot resolve is one that would enqueue an invalidation for an
// entity id nothing reads, which is indistinguishable from no rule at all
// except for the wasted outbox row. dsl validation already rejects unknown
// targets, so reaching this is a checkpoint/IR mismatch, not authored input.
func buildInboundIndex(ir *dsl.IR) map[string][]inboundRule {
	if ir == nil {
		return nil
	}
	known := make(map[string]bool, len(ir.Entities))
	for i := range ir.Entities {
		known[ir.Entities[i].ID()] = true
	}

	out := map[string][]inboundRule{}
	for i := range ir.Entities {
		parent := &ir.Entities[i]
		if parent.Cache == nil {
			continue
		}
		for _, inv := range parent.Cache.Invalidate {
			// Self-invalidation is unconditional on the write path already, so a
			// rule for it would enqueue the same invalidation twice per write.
			//
			// Both halves matter. Lowering produces Self with an empty
			// TargetID, so TargetID == "" covers the authored case alone — but
			// this IR also arrives from a persisted JSON checkpoint, where both
			// can be set. Self wins there: the field says self, and self is
			// already handled.
			if inv.Self || inv.TargetID == "" {
				continue
			}
			// A cross-entity rule needs to know which column on the child
			// carries the parent's key. Without a where-mapping there is no way
			// to identify *which* parent row to invalidate, and invalidating
			// every row of the parent entity is not what was asked for.
			if inv.Where == nil || inv.Where.Field == "" || inv.Where.SelfField == "" {
				continue
			}
			if !known[inv.TargetID] {
				continue
			}
			out[inv.TargetID] = append(out[inv.TargetID], inboundRule{
				parentEntityID: parent.ID(),
				childCol:       inv.Where.Field,
				parentCol:      inv.Where.SelfField,
			})
		}
	}
	return out
}

// inboundColumns returns the distinct child columns the rules read, sorted.
//
// Sorted because the order fixes the RETURNING clause and therefore the scan
// order, and a map-derived order would make the generated SQL — and every
// golden comparison over it — differ run to run.
func inboundColumns(rules []inboundRule) []string {
	if len(rules) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var cols []string
	for _, r := range rules {
		if !seen[r.childCol] {
			seen[r.childCol] = true
			cols = append(cols, r.childCol)
		}
	}
	sort.Strings(cols)
	return cols
}

// enqueueParents files an invalidation for every parent identified by the
// values in vals, which are the inbound columns of the written row in
// meta.inboundCols order.
//
// Runs inside the caller's transaction, so a parent invalidation is committed
// with the write that caused it or not at all. Enqueuing after commit would
// leave a window where the child is visible and the parent's cache still holds
// the pre-write row — which is the exact staleness this feature exists to
// close, reintroduced by the fix for it.
//
// A NULL parent key is skipped: the row points at no parent, so there is
// nothing to invalidate. Duplicate (parent, id) pairs are collapsed, which
// matters when a child carries two rules onto the same parent, or when the old
// and new values of an update coincide because the update did not reparent.
func (s *Server) enqueueParents(ctx context.Context, tx runtime.Tx, meta *entityMeta, valueSets ...[]any) error {
	if len(meta.inbound) == 0 {
		return nil
	}
	col := make(map[string]int, len(meta.inboundCols))
	for i, c := range meta.inboundCols {
		col[c] = i
	}

	seen := map[string]bool{}
	for _, vals := range valueSets {
		if len(vals) != len(meta.inboundCols) {
			continue
		}
		for _, r := range meta.inbound {
			// inboundCols is derived from meta.inbound, so every rule's column
			// is a key by construction; buildEntityMeta drops the whole rule
			// set if any column is unresolvable.
			i := col[r.childCol]
			v := vals[i]
			if v == nil {
				// The row points at no parent, so there is nothing to
				// invalidate. Only reachable for a nullable FK.
				continue
			}
			// No parentID == "" check: CompositeID length-prefixes, so a
			// non-nil value always yields at least "0:". A guard nothing can
			// trigger is a claim nothing checks.
			parentID := runtime.CompositeID(v)
			key := r.parentEntityID + "\x00" + parentID
			if seen[key] {
				continue
			}
			seen[key] = true

			cur, _ := s.cache.CurrentVersion(ctx, r.parentEntityID, parentID)
			if err := s.outbox.Enqueue(ctx, tx, r.parentEntityID, parentID, cur+1); err != nil {
				return err
			}
			// The parent's cached query results are keyed by a per-entity
			// generation counter, not by row, so a row-level bump alone would
			// leave a list containing the stale parent in place.
			if err := s.outbox.EnqueueGenerationBump(ctx, tx, r.parentEntityID); err != nil {
				return err
			}
		}
	}
	return nil
}

// readInboundValues runs meta.sqlSelectInbound for one row. Returns nil when
// the entity has no inbound rules, or when the row does not exist — an update
// against a missing row invalidates nothing.
func (s *Server) readInboundValues(ctx context.Context, tx runtime.Tx, meta *entityMeta, pkArgs []any) ([]any, error) {
	if meta.sqlSelectInbound == "" {
		return nil, nil
	}
	ptrs := makeScanTargets(meta.inboundColMeta)
	if err := tx.QueryRow(ctx, meta.sqlSelectInbound, pkArgs...).Scan(ptrs...); err != nil {
		// No rows is the expected case for an update against a missing row:
		// there is no old parent, and the UPDATE below reports NotFound.
		//
		// Anything else is returned. An earlier version swallowed every error
		// on the theory that invalidating one parent beat failing the write —
		// which Postgres does not allow. A genuine error (connection reset,
		// statement timeout, permission) aborts the transaction, so the next
		// statement fails with "current transaction is aborted" regardless. The
		// swallow bought nothing except discarding the root cause.
		if runtime.IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return readScanTargets(meta.inboundColMeta, ptrs), nil
}

// procedureWrittenEntities returns every entity id some procedure writes.
//
// Read-through caching is disabled for these, and that is a deliberate
// fail-closed choice rather than a limitation being tolerated.
//
// A procedure runs raw SQL steps. It declares which entities it touches, but
// nothing reports which ROWS it changed, and a row body is cached under a
// per-row version pointer — so there is no way to invalidate the bodies it
// invalidated. The generation bump the procedure does emit clears cached query
// results and leaves row bodies untouched.
//
// Before Get was served from the cache this cost nothing, because nothing was
// cached. It is live now: without this, HardDeleteProduct would delete a
// product and leave its cached body being served until TTL, and
// UpdateOrderFinancialStatusIfFresh would leave a stale order. Ten procedures
// in the shipped schemas write entities that declare read_through.
//
// Serving those entities uncached is strictly correct and merely slower. The
// alternative — caching them and hoping the TTL is short enough — is the class
// of silent staleness this whole area has been about.
func procedureWrittenEntities(ir *dsl.IR) map[string]bool {
	if ir == nil {
		return nil
	}
	out := map[string]bool{}
	for i := range ir.Procedures {
		for _, st := range ir.Procedures[i].Steps {
			if st.Raw == nil {
				continue
			}
			for _, t := range st.Raw.Touches {
				out[t] = true
			}
		}
	}
	return out
}

// parentsOf returns the entities whose invalidate_on rules name any of the
// given entity ids. Used by the procedure path, which knows which entities it
// wrote but not which rows, so it can bump the parents' generation counters
// even though it cannot invalidate their row bodies.
func parentsOf(index map[string][]inboundRule, written []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range written {
		for _, r := range index[w] {
			if !seen[r.parentEntityID] {
				seen[r.parentEntityID] = true
				out = append(out, r.parentEntityID)
			}
		}
	}
	sort.Strings(out)
	return out
}

// entityMetaFor builds one entity's meta with the indexes buildSnapshot would
// have supplied. Test helper, kept beside the indexes it wires so the two
// cannot drift.
func entityMetaFor(e *dsl.Entity, ir *dsl.IR) *entityMeta {
	return buildEntityMeta(e, ir, buildInboundIndex(ir), procedureWrittenEntities(ir))
}
