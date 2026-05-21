// Package invalidate writes the cache_invalidations outbox and drains it.
// Schema in migrations/0000_outbox.up.sql.
package invalidate

import (
	"context"
	"fmt"
	"regexp"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// schemaPat restricts schema names to a plain identifier so the
// fmt.Sprintf-built SQL in this file can't be talked into emitting
// anything but a normal table reference (review H1 / C8). Operators
// supply Schema via env; a typo or hostile config can't smuggle in
// a `; DROP TABLE` payload because it would fail this check first.
var schemaPat = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func validateSchema(s string) error {
	if !schemaPat.MatchString(s) {
		return fmt.Errorf("invalidate: schema %q is not a valid identifier", s)
	}
	return nil
}

// Outbox implements runtime.Outbox. Generated server code calls Enqueue
// inside a write tx; the row is committed atomically with the data write.
//
// Outbox has no fields beyond the table name — the runtime.Tx passed to
// Enqueue is the only state it needs.
type Outbox struct {
	// Schema lets tests / multi-tenant deploys point at an alternate schema.
	// Empty defaults to "atlantis".
	Schema string
}

// NewOutbox returns an Outbox bound to the default "atlantis" schema.
// Override Schema directly for tests or multi-tenant deploys.
func NewOutbox() *Outbox { return &Outbox{Schema: "atlantis"} }

// Enqueue records one cache invalidation inside tx. The tx must be open;
// commit/rollback is the caller's job. The trigger on the table fires
// pg_notify on commit so the worker wakes up promptly.
func (o *Outbox) Enqueue(ctx context.Context, tx runtime.Tx, entity, id string, newVersion int64) error {
	if entity == "" || id == "" {
		return fmt.Errorf("outbox: entity and id are required")
	}
	schema := o.Schema
	if schema == "" {
		schema = "atlantis"
	}
	if err := validateSchema(schema); err != nil {
		return err
	}
	const tmpl = `INSERT INTO %s.cache_invalidations (entity, row_id, new_version, kind) VALUES ($1, $2, $3, 'invalidation')`
	q := fmt.Sprintf(tmpl, schema)
	if _, err := tx.Exec(ctx, q, entity, id, newVersion); err != nil {
		return fmt.Errorf("outbox enqueue %s/%s: %w", entity, id, err)
	}
	return nil
}

// EnqueueGenerationBump records a per-entity tier-2 invalidation inside
// tx. Generated write handlers call this after Enqueue so the worker
// eventually bumps the entity's tier-2 generation counter.
//
// `row_id` and `new_version` are filled with the entity name and 0
// respectively to keep the NOT NULL constraints satisfied — the worker's
// dispatch on `kind` ignores them for this row type. The duplicated
// entity name in `row_id` lets a forensic dump distinguish bumps from
// invalidations without a JOIN.
func (o *Outbox) EnqueueGenerationBump(ctx context.Context, tx runtime.Tx, entity string) error {
	if entity == "" {
		return fmt.Errorf("outbox: entity is required")
	}
	schema := o.Schema
	if schema == "" {
		schema = "atlantis"
	}
	if err := validateSchema(schema); err != nil {
		return err
	}
	const tmpl = `INSERT INTO %s.cache_invalidations (entity, row_id, new_version, kind) VALUES ($1, $1, 0, 'generation_bump')`
	q := fmt.Sprintf(tmpl, schema)
	if _, err := tx.Exec(ctx, q, entity); err != nil {
		return fmt.Errorf("outbox generation bump %s: %w", entity, err)
	}
	return nil
}

var _ runtime.Outbox = (*Outbox)(nil)
