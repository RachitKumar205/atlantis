package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
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

	total := int64(0)
	var failures []string
	for _, e := range ir.Entities {
		if e.TtlField == "" {
			continue
		}
		schema, table := resolvePhysical(&e)

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
		if n > 0 {
			h.log().Info("sweep: deleted expired rows", "entity", e.ID(), "count", n)
			sweptTotal.WithLabelValues(e.ID()).Add(float64(n))
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
