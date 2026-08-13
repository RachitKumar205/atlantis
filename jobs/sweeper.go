package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime"
	"github.com/rachitkumar205/atlantis/internal/storage/pg"
)

// SweepExpiredJobName is the canonical id for the built-in TTL
// sweeper job. atlantis-server registers this as a handler at
// startup and inserts a job_schedules row so the sweeper fires
// periodically without any caller-side declaration.
const SweepExpiredJobName = "atlantis.SweepExpired"

// SweepExpiredArgs is empty: the sweeper reads the IR checkpoint
// at runtime to discover which entities have a ttl_field, so it
// adapts to schema changes without a redeploy.
type SweepExpiredArgs struct{}

// SweepExpiredHandler implements the built-in TTL sweeper. One
// invocation scans every entity with a ttl_field and DELETEs rows
// whose TTL column is in the past. Batched with a LIMIT to avoid
// holding a lock for too long; the scheduler will fire again on
// the next cron tick to catch remaining rows.
type SweepExpiredHandler struct {
	Pool       *pgxpool.Pool
	Logger     *slog.Logger
	BatchLimit int
}

// Handle is the jobs.Handler implementation.
func (h *SweepExpiredHandler) Handle(ctx context.Context, argsJSON []byte) error {
	// Load the current IR checkpoint to discover which entities have
	// ttl_field set. This makes the sweeper schema-aware without
	// needing a restart when an entity gains or loses a ttl_field.
	var irRaw []byte
	err := h.Pool.QueryRow(ctx, `SELECT ir FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&irRaw)
	if err != nil {
		return fmt.Errorf("sweep: load checkpoint: %w", err)
	}
	ir, err := dsl.DecodeJSONIR(irRaw)
	if err != nil {
		return fmt.Errorf("sweep: decode ir: %w", err)
	}

	limit := h.BatchLimit
	if limit <= 0 {
		limit = 1000
	}

	// Which of the tables about to be swept would hide every row from this
	// statement. Asked once, before the loop, because it is one round trip for
	// all of them.
	blocked, err := h.blockedTables(ctx, ir)
	if err != nil {
		return err
	}

	total := int64(0)
	var failures []string
	for _, e := range ir.Entities {
		if e.TtlField == "" {
			continue
		}
		schema, table := resolvePhysical(&e)

		// Skipped, loudly, rather than run into a guaranteed zero.
		//
		// Under FORCE ROW LEVEL SECURITY the sweeper's DELETE is subject to the
		// tenant policy even though the sweeper owns the table, and it binds no
		// tenant. atlantis.current_partition() returns NULL, `tenant = NULL` is
		// NULL, and the subquery selects no ctids. The DELETE then SUCCEEDS,
		// reports zero rows, and expired rows accumulate forever.
		//
		// That is the same silent shape this function's own comment below
		// describes fixing once already — "a sweeper that deleted nothing on
		// every entity for months looked exactly like a sweeper with nothing to
		// delete" — arrived at by a different route, and worse, because there is
		// no error to collect this time.
		//
		// Binding a tenant here would be wrong rather than incomplete, for the
		// reason refuseBackfillOnBlockedTables gives: expiry has to cover every
		// tenant, so there is no single correct value to bind. Enumerating
		// tenants needs a cross-tenant read, which is the thing being prevented.
		// A per-tenant sweep is a design change, and it is deferred.
		//
		// The job is NOT failed. Other entities sweep normally, and a handler
		// that dead-letters every five minutes is a handler somebody disables.
		// The counter is the durable signal.
		if blocked[schema+"."+table] {
			sweepBlockedTotal.WithLabelValues(e.ID()).Inc()
			h.log().Warn("sweep: skipped, row-level security hides every row from the sweeper",
				"entity", e.ID(),
				"table", schema+"."+table,
				"detail", "the sweeper binds no tenant, so this DELETE would succeed and "+
					"match nothing while expired rows accumulate. Remove `partition by` from "+
					"the entity, or expire its rows from the caller, which can bind a tenant")
			continue
		}

		// DELETE ... LIMIT is MySQL. Postgres rejects it outright, so for as
		// long as this statement had that shape every sweep failed on its
		// first entity — which nobody noticed, because the failure was
		// swallowed below and the job was never registered to run in the first
		// place. The bound has to be expressed as a subquery over ctid.
		//
		// Identifiers are quoted rather than interpolated bare: they come from
		// the IR checkpoint, but a `table "..."` override is author-supplied
		// text and this is a DELETE.
		sql := fmt.Sprintf(
			`DELETE FROM %s.%s WHERE ctid = ANY (ARRAY(
			     SELECT ctid FROM %s.%s WHERE %s < now() LIMIT %d))`,
			quoteIdent(schema), quoteIdent(table),
			quoteIdent(schema), quoteIdent(table), quoteIdent(e.TtlField), limit)
		tag, err := h.Pool.Exec(ctx, sql)
		if err != nil {
			// Collected, not swallowed. The previous version logged a Warn and
			// continued, so a sweeper that deleted nothing on every entity for
			// months looked exactly like a sweeper with nothing to delete.
			// Returning the error hands it to the job runtime's retry and
			// dead-letter path, where a persistent failure becomes visible.
			h.log().Error("sweep: delete failed", "entity", e.ID(), "err", err)
			failures = append(failures, fmt.Sprintf("%s: %v", e.ID(), err))
			continue
		}
		n := tag.RowsAffected()

		// Counted even when zero. Guarding the metric behind n > 0 meant an
		// entity that never deleted anything had no series at all, so "nothing
		// has expired" and "this has swept zero for a month" looked identical
		// from the outside — which is the observability half of the bug the
		// skip above handles. Logging stays behind n > 0: a line per entity per
		// five minutes is noise, a counter is not.
		sweptTotal.WithLabelValues(e.ID()).Add(float64(n))
		if n > 0 {
			h.log().Info("sweep: deleted expired rows", "entity", e.ID(), "count", n)
			total += n
		}
	}

	if total > 0 {
		_ = Checkpoint(ctx, 100, fmt.Sprintf("swept %d expired row(s)", total))
	}
	if len(failures) > 0 {
		return fmt.Errorf("sweep: %d of %d entities failed: %s",
			len(failures), len(failures)+1, strings.Join(failures, "; "))
	}
	return nil
}

// blockedTables reports which sweepable tables would hide every row from this
// sweeper, keyed by qualified name.
//
// It asks pg.RLSBlockedTables — the same catalogue query the backfill guard
// uses — rather than testing e.PartitionField != "". The declaration is not the
// authority here and gets three cases wrong, each of which that function's doc
// documents having met on a live database: a table whose `partition by` was
// added but whose policy was never emitted (sweepable, and the declaration says
// blocked); a role holding BYPASSRLS, for which no policy applies (sweepable,
// declaration says blocked); and an entity that DROPPED `partition by` while
// its policy remained (blocked, and the declaration says fine).
//
// Whether a DELETE will match rows is a property of the table and the role. So
// ask the catalogue about the table and the role.
//
// A failure here is returned rather than swallowed. Not knowing whether the
// sweep can see its rows is not a reason to sweep anyway: that is the state
// this whole change exists to stop being invisible.
func (h *SweepExpiredHandler) blockedTables(ctx context.Context, ir *dsl.IR) (map[string]bool, error) {
	var names []string
	for i := range ir.Entities {
		e := &ir.Entities[i]
		if e.TtlField == "" {
			continue
		}
		schema, table := resolvePhysical(e)
		names = append(names, schema+"."+table)
	}
	if len(names) == 0 {
		return nil, nil
	}
	list, err := pg.RLSBlockedTables(ctx, sweepQuerier{h.Pool}, names)
	if err != nil {
		return nil, fmt.Errorf("sweep: could not determine whether row-level "+
			"security would hide rows from the sweeper: %w", err)
	}
	out := make(map[string]bool, len(list))
	for _, t := range list {
		out[t] = true
	}
	return out, nil
}

// sweepQuerier adapts the pool to the narrow interface RLSBlockedTables takes.
type sweepQuerier struct{ p *pgxpool.Pool }

func (q sweepQuerier) Query(ctx context.Context, sql string, args ...any) (runtime.Rows, error) {
	return q.p.Query(ctx, sql, args...)
}

func (h *SweepExpiredHandler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// resolvePhysical returns the (schema, table) for an entity, honoring
// the table override. Mirrors introspect.physical without importing
// it to avoid a circular dependency.
func resolvePhysical(e *dsl.Entity) (schema, table string) {
	if e.TableName == "" {
		return "atlantis", e.Namespace + "_" + snakeCaseSweep(e.Name)
	}
	for i, c := range e.TableName {
		if c == '.' {
			return e.TableName[:i], e.TableName[i+1:]
		}
	}
	return "atlantis", e.TableName
}

func snakeCaseSweep(name string) string {
	var out []byte
	for i, r := range name {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				out = append(out, '_')
			}
			out = append(out, byte(r+('a'-'A')))
			continue
		}
		out = append(out, byte(r))
	}
	return string(out)
}
