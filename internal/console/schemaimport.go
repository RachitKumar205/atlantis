package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/adopt"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/dsnguard"
)

// maxImportTables bounds one pass.
//
// A schema larger than this is not refused for being large; it is refused
// because the response is read in a browser and the work happens while a
// request is held open. The message names the remedy, which is to read fewer
// schemas rather than to give up.
const maxImportTables = 500

// handleImportSchema reads a database somebody points at and returns .atl
// describing it.
//
// The one route that makes this console dial an address a request chose, with a
// credential a request supplied. dsnguard decides what may be reached; this
// decides what is done once there, and both halves matter.
//
// Scoped to the session's organisation. A generated declaration targets this
// organisation's atlantis, so the import belongs beside its schema rather than
// beside the identity that ran it.
//
// The connection string is used and dropped. It reaches no log and no table —
// console.schema_imports.source holds host and port under a CHECK — and every
// error passes through dsnguard.Redact before it leaves.
func (s *Server) handleImportSchema(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(ctxUser).(*User)

	var body struct {
		DSN       string   `json:"dsn"`
		Namespace string   `json:"namespace"`
		Schemas   []string `json:"schemas"`

		// Sent only when somebody ticked the box saying this database offers no
		// TLS. Never defaulted on: the same switch against a production
		// database sends a live password across the internet in clear.
		AllowInsecure bool `json:"allow_insecure"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		jsonError(w, "could not read the request", http.StatusBadRequest)
		return
	}

	dsn := strings.TrimSpace(body.DSN)
	if dsn == "" {
		jsonError(w, "a connection string is required", http.StatusBadRequest)
		return
	}
	ns := strings.TrimSpace(body.Namespace)
	// The namespace becomes part of every generated entity's ID, so a value
	// that does not lex is one whose .atl will not parse — refused before the
	// database is read rather than after.
	if !dsl.IsIdentifier(ns) {
		jsonError(w, "a namespace is required, and must start with a letter or "+
			"underscore and continue with letters, digits or underscores",
			http.StatusBadRequest)
		return
	}

	cfg, err := dsnguard.Config(dsn, body.AllowInsecure)
	if err != nil {
		// Refusals name what is wrong with the connection string and nothing
		// about this deployment's network.
		jsonError(w, dsnguard.Redact(err, dsn).Error(), http.StatusBadRequest)
		return
	}

	res, err := importOnce(r.Context(), cfg, ns, body.Schemas)
	if err != nil {
		s.log.Warn("schema import failed", "org", u.Org, "host", cfg.ConnConfig.Host,
			"err", dsnguard.Redact(err, dsn))

		// The driver says the server refused TLS, which reads as a broken
		// database. It is this deployment requiring TLS, and the remedy is a
		// checkbox rather than anything on their side.
		if !body.AllowInsecure && dsnguard.IsTLSRefusal(err) {
			jsonError(w, "that server offers no TLS, and atlantis requires it. "+
				"Tick \"this database has no TLS\" to read it anyway — the password "+
				"will cross the internet in clear, so use a credential that is "+
				"read-only and disposable.", http.StatusBadRequest)
			return
		}
		jsonError(w, dsnguard.Redact(err, dsn).Error(), http.StatusBadGateway)
		return
	}

	entities := make([]SchemaImportEntity, 0, len(res.Entities))
	for _, e := range res.Entities {
		entities = append(entities, SchemaImportEntity{
			Table: e.Table, Entity: e.Name, Atl: e.Atl,
		})
	}

	// Host and port, never the credential. net.JoinHostPort rather than a
	// format string, so an IPv6 literal keeps its brackets.
	source := net.JoinHostPort(cfg.ConnConfig.Host, fmt.Sprint(cfg.ConnConfig.Port))

	id, err := s.db.forOrg(u.Org).createSchemaImport(r.Context(), u.Subject, source, ns, entities)
	if err != nil {
		s.log.Error("store schema import", "org", u.Org, "err", err)
		jsonError(w, "the schema was read but could not be saved", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{
		"import_id":   id,
		"source":      source,
		"namespace":   ns,
		"entities":    res.Entities,
		"skipped":     res.Skipped,
		"warnings":    res.Warnings,
		"suggestions": res.Suggestions,
	})
}

// handleListSchemaImports returns this organisation's imports, newest first.
//
// What makes storing them worth anything: an import survives the tab that
// created it. Expired rows are excluded by the query rather than by whether the
// sweeper has run yet.
func (s *Server) handleListSchemaImports(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(ctxUser).(*User)

	imports, err := s.db.forOrg(u.Org).schemaImports(r.Context())
	if err != nil {
		s.log.Error("list schema imports", "org", u.Org, "err", err)
		jsonError(w, "could not read the imports", http.StatusInternalServerError)
		return
	}

	out := make([]map[string]any, 0, len(imports))
	for _, im := range imports {
		out = append(out, map[string]any{
			"import_id":  im.ID,
			"source":     im.Source,
			"namespace":  im.Namespace,
			"entities":   im.Entities,
			"actor":      im.Actor,
			"created_at": im.CreatedAt,
		})
	}
	jsonOK(w, map[string]any{"imports": out})
}

// handleGetSchemaImport returns the declarations of one import.
//
// An identifier from another organisation selects nothing: every statement runs
// with console.current_org() bound, and the RESTRICTIVE policy admits no other
// organisation's rows. The empty result is reported as not-found rather than as
// an empty import, so a wrong id and an id belonging to somebody else answer
// identically.
func (s *Server) handleGetSchemaImport(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(ctxUser).(*User)

	entities, err := s.db.forOrg(u.Org).schemaImportEntities(r.Context(), r.PathValue("id"))
	if err != nil {
		s.log.Error("read schema import", "org", u.Org, "err", err)
		jsonError(w, "could not read the import", http.StatusInternalServerError)
		return
	}
	if len(entities) == 0 {
		jsonError(w, "no such import", http.StatusNotFound)
		return
	}
	jsonOK(w, map[string]any{"entities": entities})
}

// importOnce opens a pool, reads, and closes it.
//
// READ ONLY at the transaction, so the guarantee is Postgres's rather than this
// package's. The advice to supply a read-only role stands alongside it: this
// stops the session writing, and the role is what stops anything else.
func importOnce(ctx context.Context, cfg *pgxpool.Config, ns string, schemas []string) (adopt.Result, error) {
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
