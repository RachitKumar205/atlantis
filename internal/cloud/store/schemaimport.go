package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// SchemaImportTTL is how long a stored import is kept.
//
// An import is a snapshot of a database Cloud does not run, held only so
// somebody can close the tab and come back to it. Keeping it past that is
// holding a customer's schema for no reason.
const SchemaImportTTL = 30 * 24 * time.Hour

// SchemaImport is one pass over a customer's database.
//
// Source is the host and port it was read from. The connection string is not
// here, is not stored anywhere, and the column carries a CHECK refusing one.
type SchemaImport struct {
	ID        string
	UserID    string
	Source    string
	Namespace string
	Entities  int
	CreatedAt time.Time
	ExpiresAt time.Time
}

// SchemaImportEntity is one generated declaration.
type SchemaImportEntity struct {
	Table  string
	Entity string
	Atl    string
}

// newImportID returns an identifier for one import.
func newImportID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate import id: %w", err)
	}
	return "imp_" + hex.EncodeToString(b), nil
}

// CreateSchemaImport stores one pass and its declarations, and returns the id.
//
// Written in a transaction: an import row with no entities reads as an empty
// database rather than as a half-finished write, and there is nothing on the
// row to tell the two apart.
//
// source must be a host, optionally with a port. A connection string fails the
// column's CHECK rather than being stored.
func (s *Store) CreateSchemaImport(ctx context.Context, userID, source, namespace string, entities []SchemaImportEntity) (string, error) {
	id, err := newImportID()
	if err != nil {
		return "", err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO cloud.schema_imports (id, user_id, source, namespace, entities, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + $6::interval)
	`, id, userID, source, namespace, len(entities), SchemaImportTTL.String()); err != nil {
		return "", fmt.Errorf("record schema import: %w", err)
	}

	for _, e := range entities {
		if _, err := tx.Exec(ctx, `
			INSERT INTO cloud.schema_import_entities (import_id, table_name, entity_name, atl)
			VALUES ($1, $2, $3, $4)
		`, id, e.Table, e.Entity, e.Atl); err != nil {
			return "", fmt.Errorf("record declaration for %s: %w", e.Table, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}

// SchemaImports lists an account's imports, newest first, excluding expired
// rows.
//
// Expiry is read here rather than trusted to a reaper: a row past its date is
// not shown even if nothing has deleted it yet.
func (s *Store) SchemaImports(ctx context.Context, userID string) ([]SchemaImport, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, source, namespace, entities, created_at, expires_at
		FROM cloud.schema_imports
		WHERE user_id = $1 AND expires_at > now()
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SchemaImport
	for rows.Next() {
		var im SchemaImport
		if err := rows.Scan(&im.ID, &im.UserID, &im.Source, &im.Namespace,
			&im.Entities, &im.CreatedAt, &im.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, im)
	}
	return out, rows.Err()
}

// SchemaImportEntities returns the declarations of one import, ordered by
// table.
//
// Scoped by userID as well as id. An identifier is not an authorisation, and
// this is the query that would otherwise hand one account another's schema.
func (s *Store) SchemaImportEntities(ctx context.Context, userID, importID string) ([]SchemaImportEntity, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.table_name, e.entity_name, e.atl
		FROM cloud.schema_import_entities e
		JOIN cloud.schema_imports i ON i.id = e.import_id
		WHERE e.import_id = $1 AND i.user_id = $2 AND i.expires_at > now()
		ORDER BY e.table_name
	`, importID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SchemaImportEntity
	for rows.Next() {
		var e SchemaImportEntity
		if err := rows.Scan(&e.Table, &e.Entity, &e.Atl); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

// DeleteExpiredSchemaImports removes rows past their date and reports how many.
//
// Entities go with them through the foreign key.
func (s *Store) DeleteExpiredSchemaImports(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM cloud.schema_imports WHERE expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
