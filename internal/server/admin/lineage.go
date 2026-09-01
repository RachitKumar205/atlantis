package admin

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// updateEntityLineage materializes per-entity and per-field blame rows
// in atlantis.entity_lineage. Each schema version creates or touches
// rows here so ownership queries are O(1) against the materialized
// table rather than requiring a full scan of schema_versions.
//
// A nil diff (seed/adopt with no prior state) is a no-op.
func updateEntityLineage(ctx context.Context, tx pgx.Tx, version int64, caller string, d *codegen.Diff) error {
	if d == nil {
		return nil
	}

	allChanges := d.All()

	for _, ch := range allChanges {
		switch ch.Kind {
		case codegen.KindEntityAdded:
			// Insert entity-level row (field_name = '').
			// ON CONFLICT handles re-add after a previous removal.
			if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.entity_lineage
    (entity_id, field_name, introduced_by, introduced_at, last_modified_by, last_modified_at, removed_at)
VALUES ($1, '', $2, $3, $2, $3, NULL)
ON CONFLICT (entity_id, field_name) DO UPDATE SET
    removed_at = NULL,
    last_modified_by = EXCLUDED.last_modified_by,
    last_modified_at = EXCLUDED.last_modified_at`,
				ch.EntityID, caller, version); err != nil {
				return err
			}

		case codegen.KindFieldAdded:
			if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.entity_lineage
    (entity_id, field_name, introduced_by, introduced_at, last_modified_by, last_modified_at, removed_at)
VALUES ($1, $2, $3, $4, $3, $4, NULL)
ON CONFLICT (entity_id, field_name) DO UPDATE SET
    removed_at = NULL,
    last_modified_by = EXCLUDED.last_modified_by,
    last_modified_at = EXCLUDED.last_modified_at`,
				ch.EntityID, ch.Field, caller, version); err != nil {
				return err
			}

		case codegen.KindEntityRemoved:
			if _, err := tx.Exec(ctx, `
UPDATE atlantis.entity_lineage
SET removed_at = $1, last_modified_by = $2, last_modified_at = $1
WHERE entity_id = $3 AND removed_at IS NULL`,
				version, caller, ch.EntityID); err != nil {
				return err
			}

		case codegen.KindFieldRemoved:
			if _, err := tx.Exec(ctx, `
UPDATE atlantis.entity_lineage
SET removed_at = $1, last_modified_by = $2, last_modified_at = $1
WHERE entity_id = $3 AND field_name = $4`,
				version, caller, ch.EntityID, ch.Field); err != nil {
				return err
			}

		default:
			// Any field modification (type changed, not-null tightened,
			// default changed, unique added/removed, reference changed,
			// serial added/removed, backfill changed, etc.): update
			// last_modified_by/at for the specific field row.
			if ch.Field != "" {
				if _, err := tx.Exec(ctx, `
UPDATE atlantis.entity_lineage
SET last_modified_by = $1, last_modified_at = $2
WHERE entity_id = $3 AND field_name = $4`,
					caller, version, ch.EntityID, ch.Field); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// lineageSeed is what an event carries when its blame rows come from the IR it
// is checkpointing rather than from a diff.
type lineageSeed struct {
	IR        *dsl.IR
	Ownership map[string]string // entity ID -> the caller that declared it
}

// seedEntityLineage writes one row per entity and one per field, from the IR
// being checkpointed.
//
// adopt is the event that needs it. An adopt's diff says what the declaration
// has and the database does not, so the entities in it are the ones
// filterToExistingEntities drops from the checkpoint: reading blame off it
// records rows for tables the checkpoint does not contain, and records nothing
// at all when the two already agree. Every entity in the baseline exists in
// the database by definition, which is the set blame is wanted for.
//
// An adopt says a table was already there. Where a row exists this only clears
// a removal, so re-running one restores what was dropped and never relabels an
// introduction it did not perform.
func seedEntityLineage(ctx context.Context, tx pgx.Tx, version int64, ir *dsl.IR, ownership map[string]string) error {
	if ir == nil {
		return nil
	}

	// One statement for the whole baseline. A 97-entity import is ~800 rows,
	// and a round trip each is the shape that took the import past the
	// console's write timeout once already.
	var entityIDs, fieldNames, callers []string
	for i := range ir.Entities {
		e := &ir.Entities[i]
		id := e.ID()
		caller := ownership[id]

		entityIDs = append(entityIDs, id)
		fieldNames = append(fieldNames, "")
		callers = append(callers, caller)

		for j := range e.Fields {
			entityIDs = append(entityIDs, id)
			fieldNames = append(fieldNames, e.Fields[j].Name)
			callers = append(callers, caller)
		}
	}
	if len(entityIDs) == 0 {
		return nil
	}

	_, err := tx.Exec(ctx, `
INSERT INTO atlantis.entity_lineage
    (entity_id, field_name, introduced_by, introduced_at, last_modified_by, last_modified_at)
SELECT t.e, t.f, t.c, $4, t.c, $4
FROM unnest($1::text[], $2::text[], $3::text[]) AS t(e, f, c)
ON CONFLICT (entity_id, field_name) DO UPDATE SET removed_at = NULL
WHERE atlantis.entity_lineage.removed_at IS NOT NULL`,
		entityIDs, fieldNames, callers, version)
	if err != nil {
		return fmt.Errorf("seed entity lineage: %w", err)
	}
	return nil
}

// normalizeActor returns a value the actor CHECK in migration 0036 accepts.
//
// The scheme carries the kind: console:usr_04f1 is a person who signed in,
// cli:rachit a shell. A value arriving without one is recorded under unknown:
// rather than refused, because the actor is provenance and nothing authorizes
// against it — failing an apply over the spelling of that field would trade a
// schema change for a label.
func normalizeActor(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, ':'); i > 0 {
		valid := true
		for k := 0; k < i; k++ {
			c := s[k]
			if c >= 'a' && c <= 'z' {
				continue
			}
			if k > 0 && c >= '0' && c <= '9' {
				continue
			}
			valid = false
			break
		}
		if valid {
			return s
		}
	}
	return "unknown:" + s
}
