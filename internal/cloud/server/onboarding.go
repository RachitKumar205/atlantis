package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/adopt"
	"github.com/rachitkumar205/atlantis/internal/cloud/dsnguard"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// maxImportTables bounds one pass.
//
// A schema larger than this is not refused for being large; it is refused
// because the response is read in a browser and the work is done while a
// request is held open.
const maxImportTables = 500

// handleIntrospectDatabase reads a database somebody points at and returns .atl
// describing it.
//
// The one route that makes Cloud dial an address a request chose, with a
// credential a request supplied. dsnguard decides what may be reached; this
// decides what is done once there, and both halves matter.
//
// The connection string is used and dropped. It reaches no log and no table —
// cloud.schema_imports.source stores the host alone, under a CHECK — and every
// error is passed through dsnguard.Redact before it leaves.
func (s *Server) handleIntrospectDatabase(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimited(w, r) {
		return
	}
	user, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if !s.sameOrigin(w, r) {
		return
	}

	// A second limit, keyed on the account rather than the address.
	// rateLimited keys on client IP, which is the wrong key for a route that
	// dials outward: an address is something an attacker rotates, and a session
	// already names somebody.
	if allowed, retry := s.lim.allow("introspect:" + user.ID); !allowed {
		w.Header().Set("Retry-After", itoa(retry))
		jsonError(w, "too many imports — try again shortly", http.StatusTooManyRequests)
		return
	}

	var body struct {
		DSN       string   `json:"dsn"`
		Namespace string   `json:"namespace"`
		Schemas   []string `json:"schemas"`
	}
	if !decode(w, r, &body) {
		return
	}

	dsn := strings.TrimSpace(body.DSN)
	if dsn == "" {
		jsonError(w, "a connection string is required", http.StatusBadRequest)
		return
	}
	ns := strings.TrimSpace(body.Namespace)
	// The namespace becomes part of every generated entity's ID, so a value
	// that does not lex is one whose .atl will not parse — refused here rather
	// than after the database has been read.
	if !dsl.IsIdentifier(ns) {
		jsonError(w, "a namespace is required, and must start with a letter or "+
			"underscore and continue with letters, digits or underscores",
			http.StatusBadRequest)
		return
	}

	cfg, err := dsnguard.Config(dsn)
	if err != nil {
		// Refusals name what is wrong with the connection string and nothing
		// about this deployment's network.
		jsonError(w, dsnguard.Redact(err, dsn).Error(), http.StatusBadRequest)
		return
	}

	res, err := introspectOnce(r.Context(), cfg, ns, body.Schemas)
	if err != nil {
		// Logged without the connection string, and answered without it.
		s.log.Warn("schema import failed", "user", user.ID, "host", cfg.ConnConfig.Host,
			"err", dsnguard.Redact(err, dsn))
		jsonError(w, dsnguard.Redact(err, dsn).Error(), http.StatusBadGateway)
		return
	}

	entities := make([]store.SchemaImportEntity, 0, len(res.Entities))
	for _, e := range res.Entities {
		entities = append(entities, store.SchemaImportEntity{
			Table: e.Table, Entity: e.Name, Atl: e.Atl,
		})
	}

	// The host and port, never the credential. net.JoinHostPort rather than a
	// format string, so an IPv6 literal keeps its brackets and the value stays
	// something a person recognises.
	source := net.JoinHostPort(cfg.ConnConfig.Host, itoa(int(cfg.ConnConfig.Port)))

	id, err := s.db.CreateSchemaImport(r.Context(), user.ID, source, ns, entities)
	if err != nil {
		s.log.Error("store schema import", "user", user.ID, "err", err)
		jsonError(w, "the schema was read but could not be saved", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"import_id":   id,
		"source":      source,
		"namespace":   ns,
		"entities":    res.Entities,
		"skipped":     res.Skipped,
		"warnings":    res.Warnings,
		"suggestions": res.Suggestions,
	})
}

// introspectOnce opens a pool, reads, and closes it.
//
// READ ONLY at the transaction, so the guarantee is Postgres's rather than this
// package's. The advice to supply a read-only role stands alongside it: this
// stops the session writing, and the role is what stops anything else.
func introspectOnce(ctx context.Context, cfg *pgxpool.Config, ns string, schemas []string) (adopt.Result, error) {
	// One connection. The pool exists because adopt takes a Querier, not
	// because anything here runs in parallel.
	cfg.MaxConns = 1

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return adopt.Result{}, fmt.Errorf("could not connect: %w", err)
	}
	defer pool.Close()

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return adopt.Result{}, fmt.Errorf("could not open a read-only transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	res, err := adopt.Generate(ctx, tx, ns, schemas, nil)
	if err != nil {
		return adopt.Result{}, err
	}
	if len(res.Entities) > maxImportTables {
		return adopt.Result{}, fmt.Errorf(
			"that database has %d tables and this reads at most %d in one pass: "+
				"name the schemas to read", len(res.Entities), maxImportTables)
	}
	return res, nil
}
