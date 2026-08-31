package console

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	ID        string    `json:"import_id"`
	Source    string    `json:"source"`
	Entities  int       `json:"entities"`
	Actor     string    `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
}

// SchemaImportEntity is one generated declaration.
//
// The shape both the import that produced it and a later read of it answer
// with: two shapes for one declaration is a browser that renders a stored
// import and a fresh one differently.
//
// Namespace is per declaration, not per import. One pass reads every schema in
// the database and gives each its own namespace, so an import spans as many as
// the database has schemas.
type SchemaImportEntity struct {
	Table     string `json:"table"`
	Entity    string `json:"entity"`
	Namespace string `json:"namespace"`
	Atl       string `json:"atl"`
}

// SchemaImportSuggestion is one change worth making to an imported table.
//
// Line is the .atl to add, and is empty where the remedy is not one line.
type SchemaImportSuggestion struct {
	Entity string `json:"entity"`
	Table  string `json:"table"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	Line   string `json:"line"`
}

// ErrNoSuchImport reports an id this organisation cannot read.
var ErrNoSuchImport = errors.New("no such import")

// SchemaImportFindings is everything one pass produced.
//
// One value rather than four parameters, because the four are written together
// and read together, and a call site that passes them positionally is one
// where skipped and warnings can be swapped without the compiler noticing.
type SchemaImportFindings struct {
	Entities    []SchemaImportEntity     `json:"entities"`
	Suggestions []SchemaImportSuggestion `json:"suggestions"`
	Skipped     []string                 `json:"skipped"`
	Warnings    []string                 `json:"warnings"`
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
func (o *orgStore) createSchemaImport(ctx context.Context, actor, source string, f SchemaImportFindings) (string, error) {
	id, err := newSchemaImportID()
	if err != nil {
		return "", err
	}

	err = o.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO console.schema_imports
			    (id, org, source, entities, actor, expires_at, suggestions, skipped, warnings)
			VALUES ($1, $2, $3, $4, $5, now() + $6::interval, $7, $8, $9)
		`, id, o.org, source, len(f.Entities), actor, schemaImportTTL.String(),
			orEmpty(f.Suggestions), orEmpty(f.Skipped), orEmpty(f.Warnings)); err != nil {
			return fmt.Errorf("record schema import: %w", err)
		}
		for _, e := range f.Entities {
			if _, err := tx.Exec(ctx, `
				INSERT INTO console.schema_import_entities
				    (import_id, org, table_name, entity_name, namespace, atl)
				VALUES ($1, $2, $3, $4, $5, $6)
			`, id, o.org, e.Table, e.Entity, e.Namespace, e.Atl); err != nil {
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
			SELECT id, source, entities, actor, created_at
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
			if err := rows.Scan(&im.ID, &im.Source, &im.Entities,
				&im.Actor, &im.CreatedAt); err != nil {
				return err
			}
			out = append(out, im)
		}
		return rows.Err()
	})
	return out, err
}

// SchemaImportOverview is an import without its payload.
//
// Counts and namespace names, which is everything the review screen needs to
// draw its header and its sections. The declarations and the findings are
// several hundred kilobytes together and are fetched per section.
type SchemaImportOverview struct {
	ID          string           `json:"import_id"`
	Source      string           `json:"source"`
	Actor       string           `json:"actor"`
	CreatedAt   time.Time        `json:"created_at"`
	Entities    int              `json:"entities"`
	Namespaces  []NamespaceCount `json:"namespaces"`
	Suggestions int              `json:"suggestions"`
	Skipped     int              `json:"skipped"`
	Warnings    int              `json:"warnings"`
}

// NamespaceCount is one namespace an import read into, and how many tables it
// found there.
type NamespaceCount struct {
	Name   string `json:"name"`
	Tables int    `json:"tables"`
}

// SchemaImportNotes is what a pass found, without the declarations.
type SchemaImportNotes struct {
	Suggestions []SchemaImportSuggestion `json:"suggestions"`
	Skipped     []string                 `json:"skipped"`
	Warnings    []string                 `json:"warnings"`
}

// schemaImportOverview returns the header and the counts.
//
// jsonb_array_length rather than reading the arrays: the counts are what the
// screen draws, and the arrays behind them are the bulk of the response this
// call exists to avoid.
func (o *orgStore) schemaImportOverview(ctx context.Context, importID string) (*SchemaImportOverview, error) {
	var v SchemaImportOverview
	err := o.tx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT id, source, actor, created_at, entities,
			       jsonb_array_length(suggestions),
			       jsonb_array_length(skipped),
			       jsonb_array_length(warnings)
			FROM console.schema_imports
			WHERE id = $1 AND expires_at > now()
		`, importID).Scan(&v.ID, &v.Source, &v.Actor, &v.CreatedAt, &v.Entities,
			&v.Suggestions, &v.Skipped, &v.Warnings)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSuchImport
		}
		if err != nil {
			return err
		}

		rows, err := tx.Query(ctx, `
			SELECT namespace, count(*)
			FROM console.schema_import_entities
			WHERE import_id = $1
			GROUP BY namespace
			ORDER BY namespace
		`, importID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n NamespaceCount
			if err := rows.Scan(&n.Name, &n.Tables); err != nil {
				return err
			}
			v.Namespaces = append(v.Namespaces, n)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// schemaImportEntities returns the declarations of one import, ordered by
// table. Optionally narrowed to one namespace, which is how the review screen
// reads them: one file's worth at a time.
func (o *orgStore) schemaImportEntities(ctx context.Context, importID, namespace string) ([]SchemaImportEntity, error) {
	var out []SchemaImportEntity
	err := o.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT table_name, entity_name, namespace, atl
			FROM console.schema_import_entities
			WHERE import_id = $1 AND ($2 = '' OR namespace = $2)
			ORDER BY table_name
		`, importID, namespace)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e SchemaImportEntity
			if err := rows.Scan(&e.Table, &e.Entity, &e.Namespace, &e.Atl); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// schemaImportSource returns the host and port an import was read from.
//
// Its own read because the declarations endpoint needs the source and nothing
// else on the row, and the row carries the findings — which are the bulk of it.
func (o *orgStore) schemaImportSource(ctx context.Context, importID string) (string, error) {
	var source string
	err := o.tx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT source FROM console.schema_imports
			WHERE id = $1 AND expires_at > now()
		`, importID).Scan(&source)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSuchImport
		}
		return err
	})
	return source, err
}

// schemaImportNotes returns the suggestions, the skipped tables and the
// warnings.
func (o *orgStore) schemaImportNotes(ctx context.Context, importID string) (*SchemaImportNotes, error) {
	var n SchemaImportNotes
	err := o.tx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT suggestions, skipped, warnings
			FROM console.schema_imports
			WHERE id = $1 AND expires_at > now()
		`, importID).Scan(&n.Suggestions, &n.Skipped, &n.Warnings)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSuchImport
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// deleteExpiredSchemaImports removes rows past their date and reports how many.
//
// Entities go with them through the foreign key: referential integrity actions
// bypass row security, so the cascade needs no bind of its own.
//
// Organisation by organisation, because the table is under a RESTRICTIVE policy
// that admits nothing to an unbound session. See store.eachOrg.
func (s *store) deleteExpiredSchemaImports(ctx context.Context) (int64, error) {
	var total int64
	err := s.eachOrg(ctx, func(o *orgStore) error {
		return o.tx(ctx, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx,
				`DELETE FROM console.schema_imports WHERE expires_at <= now()`)
			if err != nil {
				return err
			}
			total += tag.RowsAffected()
			return nil
		})
	})
	return total, err
}
