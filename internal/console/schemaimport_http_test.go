package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The refusal the browser has to act on rather than print.
//
// A database offering no TLS is answered by asking whether to send the
// password in clear and sending the request again with allow_insecure. The SPA
// finds that case by the code, so a refusal that loses it leaves the dialog
// unopened and the sentence unanswerable — the same shape as sudo_required on
// the approve route.

func importRefusal(t *testing.T, f *consoleFixture, body string) (int, map[string]string) {
	t.Helper()
	token := f.signIn(t, "admin@example.com", "admin")
	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST", "/api/schema/import", body, token))

	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("the refusal is not JSON: %v. Body: %s", err, w.Body.String())
	}
	return w.Code, got
}

func TestImportRefusesADSNWithTLSDisabledAndSaysWhy(t *testing.T) {
	f := newConsoleFixture(t)

	code, body := importRefusal(t, f,
		`{"dsn":"postgres://u:p@db.example.com:5432/app?sslmode=disable"}`)

	if code != http.StatusBadRequest {
		t.Errorf("sslmode=disable got %d, want 400", code)
	}
	if body["code"] != codeTLSRequired {
		t.Errorf("code = %q, want %q. Without it the SPA cannot tell this refusal "+
			"from an unreachable host, so it prints the sentence and offers nothing",
			body["code"], codeTLSRequired)
	}
	// The sentence stands on its own for a client that ignores the code.
	if body["error"] == "" {
		t.Error("the refusal carries a code and no message")
	}
}

// The password must not survive into the body that carries the code. Redact
// runs on the guard's error, and the code path added beside it is a second
// place that could return the raw error.
func TestImportRefusalDoesNotEchoTheConnectionString(t *testing.T) {
	f := newConsoleFixture(t)

	dsn := "postgres://u:hunter2@db.example.com:5432/app?sslmode=disable"
	_, body := importRefusal(t, f, `{"dsn":"`+dsn+`"}`)

	for _, leak := range []string{"hunter2", dsn} {
		if strings.Contains(body["error"], leak) {
			t.Errorf("the refusal quotes %q back: %s", leak, body["error"])
		}
	}
}

// A refusal that is not about TLS carries no code, so the SPA prints it rather
// than opening a dialog offering to retry something that cannot be retried.
func TestImportRefusesAUnixSocketWithoutTheTLSCode(t *testing.T) {
	f := newConsoleFixture(t)

	code, body := importRefusal(t, f, `{"dsn":"postgres://u:p@/app?host=/var/run/postgresql"}`)

	if code != http.StatusBadRequest {
		t.Errorf("a unix socket host got %d, want 400", code)
	}
	if body["code"] == codeTLSRequired {
		t.Error("a unix-socket refusal carries tls_required, so the browser offers " +
			"to retry it in clear — which would not make a local socket reachable")
	}
}

func TestImportRefusesAnEmptyConnectionString(t *testing.T) {
	f := newConsoleFixture(t)

	code, body := importRefusal(t, f, `{"dsn":"   "}`)

	if code != http.StatusBadRequest {
		t.Errorf("an empty connection string got %d, want 400", code)
	}
	if body["code"] == codeTLSRequired {
		t.Error("an empty connection string reports a TLS problem")
	}
}

// Reading one import back over HTTP.
//
// The review screen is a URL naming an import, so this is the request that
// renders it. It was not covered: the store had tests, the route had none, and
// a screen stuck on "Loading" is what a hang here looks like.
func TestGetSchemaImportAnswersTheOverview(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	id, err := f.srv.db.forOrg(orgAcme).createSchemaImport(ctx,
		subjectFor(orgAcme, "a@example.com"), "db.example.com:5432",
		SchemaImportFindings{
			Entities: sampleEntities("acme"),
			Suggestions: []SchemaImportSuggestion{
				{Entity: "acmeOrder", Table: "public.acme_orders",
					Kind: "no-primary-key", Detail: "no primary key", Line: ""},
			},
			Skipped:  []string{"public.legacy: reserved column name"},
			Warnings: []string{"index on public.acme_orders could not be spelled"},
		})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	token := f.signIn(t, "admin@example.com", "admin")
	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "GET", "/api/schema/imports/"+id, "", token))

	if w.Code != http.StatusOK {
		t.Fatalf("GET the import got %d, want 200. Body: %s", w.Code, w.Body.String())
	}

	var got struct {
		ImportID    string `json:"import_id"`
		Source      string `json:"source"`
		Entities    int    `json:"entities"`
		Suggestions int    `json:"suggestions"`
		Skipped     int    `json:"skipped"`
		Warnings    int    `json:"warnings"`
		Namespaces  []struct {
			Name   string `json:"name"`
			Tables int    `json:"tables"`
		} `json:"namespaces"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v. Body: %s", err, w.Body.String())
	}
	if got.ImportID != id {
		t.Errorf("import_id = %q, want %q", got.ImportID, id)
	}
	if got.Entities != 2 || got.Suggestions != 1 || got.Skipped != 1 || got.Warnings != 1 {
		t.Errorf("counts = entities %d, suggestions %d, skipped %d, warnings %d; "+
			"want 2, 1, 1, 1", got.Entities, got.Suggestions, got.Skipped, got.Warnings)
	}
	// sampleEntities spans public and sales, so a single namespace here means
	// the grouping collapsed them.
	if len(got.Namespaces) != 2 {
		t.Errorf("%d namespaces, want 2: %+v", len(got.Namespaces), got.Namespaces)
	}

	// The overview carries no .atl. That is the whole point of the split: the
	// declarations were 77 kB on the pass this was written against, and the
	// header is drawn before anybody opens them.
	if strings.Contains(w.Body.String(), "entity acmeOrder") {
		t.Error("the overview carries declaration bodies, so it is as large as " +
			"the response it replaced")
	}
}

// The declarations, fetched on their own and narrowable to one namespace.
func TestGetSchemaImportEntitiesNarrowsToANamespace(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	id, err := f.srv.db.forOrg(orgAcme).createSchemaImport(ctx,
		subjectFor(orgAcme, "a@example.com"), "db.example.com:5432",
		sampleFindings("acme"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	token := f.signIn(t, "admin@example.com", "admin")

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", 2},
		{"?namespace=public", 1},
		{"?namespace=sales", 1},
		{"?namespace=nosuch", 0},
	} {
		w := httptest.NewRecorder()
		f.srv.handler.ServeHTTP(w, f.request(t, "GET",
			"/api/schema/imports/"+id+"/entities"+tc.query, "", token))
		if w.Code != http.StatusOK {
			t.Fatalf("%q got %d, want 200. Body: %s", tc.query, w.Code, w.Body.String())
		}
		var got struct {
			Entities []any `json:"entities"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%q decode: %v", tc.query, err)
		}
		if len(got.Entities) != tc.want {
			t.Errorf("%q returned %d declarations, want %d", tc.query, len(got.Entities), tc.want)
		}
	}
}

func TestGetSchemaImportNotesAnswersTheFindings(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	id, err := f.srv.db.forOrg(orgAcme).createSchemaImport(ctx,
		subjectFor(orgAcme, "a@example.com"), "db.example.com:5432",
		SchemaImportFindings{
			Entities: sampleEntities("acme"),
			Suggestions: []SchemaImportSuggestion{
				{Entity: "acmeOrder", Table: "public.acme_orders", Kind: "no-primary-key"},
			},
			Skipped:  []string{"public.legacy: reserved column name"},
			Warnings: []string{"an index could not be spelled"},
		})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	token := f.signIn(t, "admin@example.com", "admin")

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "GET",
		"/api/schema/imports/"+id+"/notes", "", token))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	var got struct {
		Suggestions []any    `json:"suggestions"`
		Skipped     []string `json:"skipped"`
		Warnings    []string `json:"warnings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Suggestions) != 1 || len(got.Skipped) != 1 || len(got.Warnings) != 1 {
		t.Errorf("suggestions %d, skipped %d, warnings %d; want 1 each",
			len(got.Suggestions), len(got.Skipped), len(got.Warnings))
	}
}

func TestGetSchemaImportIsNotFoundForAnUnknownID(t *testing.T) {
	f := newConsoleFixture(t)
	token := f.signIn(t, "admin@example.com", "admin")

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "GET", "/api/schema/imports/imp_nope", "", token))

	if w.Code != http.StatusNotFound {
		t.Errorf("an unknown id got %d, want 404. Body: %s", w.Code, w.Body.String())
	}
}

// Plan and apply are the two calls here that reach the organisation's own
// atlantis, so their refusals are asserted individually.
//
// Neither takes a caller. ApplyMigration binds req.caller to the authenticated
// certificate CN, so the console can only apply as itself; a browser-supplied
// name was refused by the server with "does not match authenticated identity".

func TestImportPlanAndApplySubmitAsTheConsole(t *testing.T) {
	// The constant the handlers send. It is reserved — the signer refuses to
	// issue this CN to anyone else — and migrations 0018, 0019 and 0034 grant
	// its capabilities by that name, so a rename here silently loses them.
	if consoleCaller != "atlantis-console" {
		t.Errorf("consoleCaller = %q; the grants in migrations/infra name "+
			"'atlantis-console', so this identity holds no capabilities", consoleCaller)
	}
}

func TestImportPlanIsNotFoundForAnUnknownImport(t *testing.T) {
	f := newConsoleFixture(t)
	token := f.signIn(t, "admin@example.com", "admin")

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST",
		"/api/schema/imports/imp_nope/plan", "", token))

	if w.Code != http.StatusNotFound {
		t.Errorf("an unknown import got %d, want 404. Body: %s", w.Code, w.Body.String())
	}
}

// Committing an import registers it; it does not create anything.
//
// The endpoint takes no plan and no caller. AdoptBaseline checks the
// declarations against the managed database and writes the checkpoint, so
// there is nothing for the browser to have been shown first.
func TestImportCommitIsNotFoundForAnUnknownImport(t *testing.T) {
	f := newConsoleFixture(t)
	token := f.signIn(t, "admin@example.com", "admin")

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST",
		"/api/schema/imports/imp_nope/apply", "{}", token))

	if w.Code != http.StatusNotFound {
		t.Errorf("an unknown import got %d, want 404. Body: %s", w.Code, w.Body.String())
	}
}

// The declarations are grouped the way the browser shows them: one file per
// namespace, sorted.
func TestImportFilesAreOnePerNamespace(t *testing.T) {
	files := importFiles([]SchemaImportEntity{
		{Table: "sales.b", Entity: "B", Namespace: "sales", Atl: "entity B in sales {}"},
		{Table: "public.a", Entity: "A", Namespace: "public", Atl: "entity A in public {}"},
		{Table: "public.c", Entity: "C", Namespace: "public", Atl: "entity C in public {}"},
	})

	if len(files) != 2 {
		t.Fatalf("%d files, want 2 (one per namespace): %+v", len(files), files)
	}
	if files[0].GetPath() != "schema/public.atl" || files[1].GetPath() != "schema/sales.atl" {
		t.Errorf("paths = %q, %q; want schema/public.atl, schema/sales.atl "+
			"— unsorted files make two adopts of one import disagree",
			files[0].GetPath(), files[1].GetPath())
	}
	if got := string(files[0].GetContent()); got != "entity A in public {}\n\nentity C in public {}" {
		t.Errorf("public.atl = %q; the namespace's declarations are not joined in order", got)
	}
}
