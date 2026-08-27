package console

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// schemaImportTTL is how long a stored import is kept.
//
// An import is a snapshot of a database this console does not run, held only so
// somebody can close the tab and come back to it. Keeping it past that is
// holding a customer's schema for no reason.
const schemaImportTTL = 30 * 24 * time.Hour

// SchemaImport is one pass over a database.
//
// Source is the host and port it was read from. The connection string is not
// here, is not stored anywhere, and the column carries a CHECK refusing one.
type SchemaImport struct {
	ID        string
	Source    string
	Namespace string
	Entities  int
	Actor     string
	CreatedAt time.Time
}

// SchemaImportEntity is one generated declaration.
type SchemaImportEntity struct {
	Table  string
	Entity string
	Atl    string
}

// newSchemaImportID returns an identifier for one import.
func newSchemaImportID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate import id: %w", err)
	}
	return "imp_" + hex.EncodeToString(b), nil
}

// createSchemaImport stores one pass and its declarations, and returns the id.
//
// One transaction, which orgScope.tx binds the organisation on: an import row
// with no entities reads as an empty database rather than as a half-finished
// write, and nothing on the row tells the two apart.
//
// source must be a host, optionally with a port. A connection string fails the
// column's CHECK rather than being stored.
func (o *orgStore) createSchemaImport(ctx context.Context, actor, source, namespace string, entities []SchemaImportEntity) (string, error) {
	id, err := newSchemaImportID()
	if err != nil {
		return "", err
	}

	err = o.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO console.schema_imports (id, org, source, namespace, entities, actor, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, now() + $7::interval)
		`, id, o.org, source, namespace, len(entities), actor, schemaImportTTL.String()); err != nil {
			return fmt.Errorf("record schema import: %w", err)
		}
		for _, e := range entities {
			if _, err := tx.Exec(ctx, `
				INSERT INTO console.schema_import_entities (import_id, org, table_name, entity_name, atl)
				VALUES ($1, $2, $3, $4, $5)
			`, id, o.org, e.Table, e.Entity, e.Atl); err != nil {
				return fmt.Errorf("record declaration for %s: %w", e.Table, err)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// schemaImports lists this organisation's imports, newest first, excluding
// expired rows.
//
// Expiry is read here rather than trusted to a reaper: a row past its date is
// not shown even if nothing has deleted it yet.
func (o *orgStore) schemaImports(ctx context.Context) ([]SchemaImport, error) {
	var out []SchemaImport
	err := o.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, source, namespace, entities, actor, created_at
			FROM console.schema_imports
			WHERE expires_at > now()
			ORDER BY created_at DESC
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var im SchemaImport
			if err := rows.Scan(&im.ID, &im.Source, &im.Namespace, &im.Entities,
				&im.Actor, &im.CreatedAt); err != nil {
				return err
			}
			out = append(out, im)
		}
		return rows.Err()
	})
	return out, err
}

// schemaImportEntities returns the declarations of one import, ordered by
// table.
//
// The organisation is not in the WHERE clause because the policy is: every
// statement here runs with console.current_org() bound, and the RESTRICTIVE
// policy admits no other organisation's rows. An id from elsewhere selects
// nothing.
func (o *orgStore) schemaImportEntities(ctx context.Context, importID string) ([]SchemaImportEntity, error) {
	var out []SchemaImportEntity
	err := o.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT table_name, entity_name, atl
			FROM console.schema_import_entities
			WHERE import_id = $1
			ORDER BY table_name
		`, importID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e SchemaImportEntity
			if err := rows.Scan(&e.Table, &e.Entity, &e.Atl); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// deleteExpiredSchemaImports removes rows past their date and reports how many.
//
// Entities go with them through the foreign key. Not organisation-scoped: it
// runs on a timer with no request behind it, and every organisation's expired
// rows are equally expired.
func (s *store) deleteExpiredSchemaImports(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM console.schema_imports WHERE expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
