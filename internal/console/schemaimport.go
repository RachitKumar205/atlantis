package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/adopt"
	"github.com/rachitkumar205/atlantis/internal/dsl/atlemit"
	"github.com/rachitkumar205/atlantis/internal/dsnguard"
)

// maxImportTables bounds one pass.
//
// A schema larger than this is not refused for being large; it is refused
// because the response is read in a browser and the work happens while a
// request is held open. The message names the remedy, which is to read fewer
// schemas rather than to give up.
const maxImportTables = 500

// errTooManyTables reports a pass that exceeded maxImportTables.
//
// Answered as a 400 beside adopt.ErrNothingToRead: both describe the schemas
// the request named, and neither says anything about reaching the database.
var errTooManyTables = errors.New("too many tables")

// codeTLSRequired marks the one refusal a second request can settle: the
// database offers no TLS, and allow_insecure decides whether to read it.
//
// Both refusals carry it — the DSN that disables TLS before anything is
// dialled, and the server that turns the attempt down.
const codeTLSRequired = "tls_required"

// consoleCaller is the identity this console applies as.
//
// Not a choice the browser makes. ApplyMigration binds req.caller to the
// authenticated certificate CN — SCHEMA_APPLY means "may write to my schema",
// not "any caller's" — so naming anything else is refused by the server.
//
// The name is reserved rather than conventional: the signer refuses to issue
// this CN to anyone else (SIGNER_ALLOWED_CLIENT_CNS), and migrations 0018,
// 0019 and 0034 grant its capabilities by that name.
const consoleCaller = "atlantis-console"

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
// Naming a database here is asking atlantis to manage it, so the connection
// string is handed to the organisation's own server, which seals it in
// atlantis.managed_database.
//
// It is not stored by this console: console.schema_imports.source holds host
// and port under a CHECK refusing a credential, it reaches no log, and every
// error passes through dsnguard.Redact before it leaves.
func (s *Server) handleImportSchema(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(ctxUser).(*User)

	var body struct {
		DSN string `json:"dsn"`

		// Sent only on a second request, after a refusal carrying
		// codeTLSRequired was answered. Never defaulted on: the same switch
		// against a production database sends a live password across the
		// internet in clear.
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

	cfg, err := dsnguard.Config(dsn, body.AllowInsecure)
	if err != nil {
		// A DSN that disables TLS outright is answerable: the browser asks
		// whether to send the password in clear and sends the request again.
		// codeTLSRequired is what tells it which refusal this is.
		if errors.Is(err, dsnguard.ErrNoTLS) {
			jsonErrorCode(w, dsnguard.Redact(err, dsn).Error(),
				codeTLSRequired, http.StatusBadRequest)
			return
		}
		// Refusals name what is wrong with the connection string and nothing
		// about this deployment's network.
		jsonError(w, dsnguard.Redact(err, dsn).Error(), http.StatusBadRequest)
		return
	}

	res, err := importOnce(r.Context(), cfg)
	if err != nil {
		s.log.Warn("schema import failed", "org", u.Org, "host", cfg.ConnConfig.Host,
			"err", dsnguard.Redact(err, dsn))

		// The driver says the server refused TLS, which reads as a broken
		// database. It is this deployment requiring TLS, and the remedy is a
		// second request rather than anything on their side.
		if !body.AllowInsecure && dsnguard.IsTLSRefusal(err) {
			jsonErrorCode(w, "that server offers no TLS, and atlantis requires it. "+
				"Reading it anyway sends the password across the internet in clear, "+
				"so use a credential that is read-only and disposable.",
				codeTLSRequired, http.StatusBadRequest)
			return
		}
		// What the request asked for, rather than a failure to reach the
		// database. 502 here would blame the customer's server for a schema
		// name typed on this form, and send them looking at the wrong machine.
		if errors.Is(err, adopt.ErrNothingToRead) || errors.Is(err, errTooManyTables) {
			jsonError(w, dsnguard.Redact(err, dsn).Error(), http.StatusBadRequest)
			return
		}
		jsonError(w, dsnguard.Redact(err, dsn).Error(), http.StatusBadGateway)
		return
	}

	entities := make([]SchemaImportEntity, 0, len(res.Entities))
	for _, e := range res.Entities {
		entities = append(entities, SchemaImportEntity{
			Table: e.Table, Entity: e.Name, Namespace: e.Namespace, Atl: e.Atl,
		})
	}
	suggestions := make([]SchemaImportSuggestion, 0, len(res.Suggestions))
	for _, sg := range res.Suggestions {
		suggestions = append(suggestions, SchemaImportSuggestion{
			Entity: sg.Entity, Table: sg.Table, Kind: sg.Kind,
			Detail: sg.Detail, Line: sg.Line,
		})
	}

	// Host and port, never the credential. net.JoinHostPort rather than a
	// format string, so an IPv6 literal keeps its brackets.
	source := net.JoinHostPort(cfg.ConnConfig.Host, fmt.Sprint(cfg.ConnConfig.Port))

	findings := SchemaImportFindings{
		Entities:    entities,
		Suggestions: suggestions,
		Skipped:     orEmpty(res.Skipped),
		Warnings:    orEmpty(res.Warnings),
	}

	id, err := s.db.forOrg(u.Org).createSchemaImport(r.Context(), u.Subject, source, findings)
	if err != nil {
		s.log.Error("store schema import", "org", u.Org, "err", err)
		jsonError(w, "the schema was read but could not be saved", http.StatusInternalServerError)
		return
	}

	// Naming a database here is asking atlantis to manage it, so the
	// connection string goes to the organisation's own server, which seals it.
	// It is not stored by this console: console.schema_imports.source carries
	// a CHECK refusing a credential, and that has not changed.
	//
	// A failure here leaves the import readable and the database unmanaged,
	// which the review screen reports when it plans. Reported rather than
	// fatal: the declarations were read and are worth keeping either way.
	if atl, aerr := s.atlFor(r.Context(), u.Org); aerr != nil {
		s.log.Error("resolve atlantis to set the managed database",
			"org", u.Org, "err", aerr)
	} else if _, err := atl.SetManagedDatabase(r.Context(), &adminpb.SetManagedDatabaseRequest{
		Dsn:    dsn,
		Source: source,
		SetBy:  u.Subject,
	}); err != nil {
		s.log.Error("set managed database", "org", u.Org, "host", source, "err", err)
	}

	// The identifier is what the browser needs: the review is a screen at a URL
	// naming this import, and it reads the rest back through handleGetSchemaImport.
	jsonOK(w, map[string]any{"import_id": id})
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

	jsonOK(w, map[string]any{"imports": orEmpty(imports)})
}

// handleGetSchemaImport returns one import's header and its counts.
//
// An identifier from another organisation selects nothing: every statement runs
// with console.current_org() bound, and the RESTRICTIVE policy admits no other
// organisation's rows. The empty result is reported as not-found rather than as
// an empty import, so a wrong id and an id belonging to somebody else answer
// identically.
func (s *Server) handleGetSchemaImport(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(ctxUser).(*User)

	v, err := s.db.forOrg(u.Org).schemaImportOverview(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNoSuchImport) {
		jsonError(w, "no such import", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("read schema import", "org", u.Org, "err", err)
		jsonError(w, "could not read the import", http.StatusInternalServerError)
		return
	}
	v.Namespaces = orEmpty(v.Namespaces)
	jsonOK(w, v)
}

// handleGetSchemaImportEntities returns the declarations, optionally narrowed
// to one namespace.
//
// Separate from the overview because these are the bulk of an import — 84
// tables came to 77 kB of .atl on the pass this was written against — and the
// review screen draws its header before anybody opens them.
func (s *Server) handleGetSchemaImportEntities(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(ctxUser).(*User)

	id := r.PathValue("id")
	ents, err := s.db.forOrg(u.Org).schemaImportEntities(
		r.Context(), id, r.URL.Query().Get("namespace"))
	if err != nil {
		s.log.Error("read schema import declarations", "org", u.Org, "err", err)
		jsonError(w, "could not read the declarations", http.StatusInternalServerError)
		return
	}

	// The comment a file opens with. The browser groups the declarations into
	// files and prepends this to each; composing it here keeps one copy of the
	// words in the tree that generates them.
	source, err := s.db.forOrg(u.Org).schemaImportSource(r.Context(), id)
	if err != nil && !errors.Is(err, ErrNoSuchImport) {
		s.log.Error("read schema import source", "org", u.Org, "err", err)
	}

	// An import with no declarations is not distinguished from an id that
	// selects nothing: the overview is what reports whether the import exists.
	jsonOK(w, map[string]any{
		"entities": orEmpty(ents),
		"header":   atlemit.Header(source),
	})
}

// handleGetSchemaImportNotes returns the suggestions, the skipped tables and
// the warnings.
func (s *Server) handleGetSchemaImportNotes(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(ctxUser).(*User)

	n, err := s.db.forOrg(u.Org).schemaImportNotes(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNoSuchImport) {
		jsonError(w, "no such import", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("read schema import notes", "org", u.Org, "err", err)
		jsonError(w, "could not read the notes", http.StatusInternalServerError)
		return
	}
	n.Suggestions = orEmpty(n.Suggestions)
	n.Skipped = orEmpty(n.Skipped)
	n.Warnings = orEmpty(n.Warnings)
	jsonOK(w, n)
}

// orEmpty returns a slice that marshals to [] rather than null.
//
// A nil slice reaches the browser as null, and the pages index and measure
// these without a guard: null.length is a TypeError that blanks the screen.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// importOnce opens a pool, reads, and closes it.
//
// READ ONLY at the transaction, so the guarantee is Postgres's rather than this
// package's. The advice to supply a read-only role stands alongside it: this
// stops the session writing, and the role is what stops anything else.
func importOnce(ctx context.Context, cfg *pgxpool.Config) (adopt.Result, error) {
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

	res, err := adopt.GenerateAll(ctx, tx, nil)
	if err != nil {
		return adopt.Result{}, err
	}
	if len(res.Entities) > maxImportTables {
		return adopt.Result{}, fmt.Errorf(
			"%w: that database has %d tables and this reads at most %d in one pass",
			errTooManyTables, len(res.Entities), maxImportTables)
	}
	return res, nil
}

// handlePlanImport reports what applying an import's declarations would do.
//
// AdoptBaseline is the wrong operation here and was tried first. Adopt
// baselines a database atlantis already manages, filtering the checkpoint to
// entities that physically exist; an import describes tables in somebody
// else's database, so adopt recorded 671 absent entities, wrote an empty
// checkpoint, and left the schema page blank.
//
// These tables have to be created. Plan says what the DDL would be and writes
// nothing; handleApplyImport runs it.
func (s *Server) handlePlanImport(w http.ResponseWriter, r *http.Request) {
	ents, ok := s.importDeclarations(w, r)
	if !ok {
		return
	}
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.PlanSchema(r.Context(), &adminpb.PlanSchemaRequest{
		Caller: consoleCaller,
		Files:  importFiles(ents),
	})
	s.proxyProto(w, "PlanSchema", resp, err)
}

// handleApplyImport registers an import's declarations as the schema.
//
// AdoptBaseline, not ApplyMigration. The tables are already there — that is
// what an import read — so what is missing is atlantis counting them as its
// own. Adopt introspects the managed database, checks the declarations
// against it, and writes the checkpoint. No DDL runs.
//
// ApplyMigration is the wrong half here and was tried: it diffs against the
// checkpoint rather than the live catalogue, so an empty checkpoint made it
// plan 183 CREATE statements for tables that exist.
func (s *Server) handleApplyImport(w http.ResponseWriter, r *http.Request) {
	ents, ok := s.importDeclarations(w, r)
	if !ok {
		return
	}

	u := r.Context().Value(ctxUser).(*User)
	actor, actorEmail := u.Actor()
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	// allow_drift is not exposed. Adopt refuses when introspection disagrees
	// with the declarations, and that refusal is the safety of the button:
	// baselining a schema that does not match makes every later plan compare
	// against a checkpoint describing a database nobody has.
	resp, err := atl.AdoptBaseline(r.Context(), &adminpb.AdoptBaselineRequest{
		Caller:         consoleCaller,
		Files:          importFiles(ents),
		AdoptedBy:      actor,
		AdoptedByEmail: actorEmail,
	})
	s.proxyProto(w, "AdoptBaseline", resp, err)
}

// importDeclarations reads the declarations both halves submit.
func (s *Server) importDeclarations(w http.ResponseWriter, r *http.Request) ([]SchemaImportEntity, bool) {
	u := r.Context().Value(ctxUser).(*User)

	ents, err := s.db.forOrg(u.Org).schemaImportEntities(r.Context(), r.PathValue("id"), "")
	if err != nil {
		s.log.Error("read schema import for apply", "org", u.Org, "err", err)
		jsonError(w, "could not read the declarations", http.StatusInternalServerError)
		return nil, false
	}
	if len(ents) == 0 {
		jsonError(w, "no such import", http.StatusNotFound)
		return nil, false
	}
	return ents, true
}

// importFiles groups declarations into the files the server receives: one per
// namespace, matching what the browser shows.
func importFiles(ents []SchemaImportEntity) []*adminpb.SubmittedFile {
	bodies := make(map[string][]string)
	for _, e := range ents {
		bodies[e.Namespace] = append(bodies[e.Namespace], e.Atl)
	}
	names := make([]string, 0, len(bodies))
	for ns := range bodies {
		names = append(names, ns)
	}
	sort.Strings(names)

	out := make([]*adminpb.SubmittedFile, 0, len(names))
	for _, ns := range names {
		out = append(out, &adminpb.SubmittedFile{
			Path:    "schema/" + ns + ".atl",
			Content: []byte(strings.Join(bodies[ns], "\n\n")),
		})
	}
	return out
}
