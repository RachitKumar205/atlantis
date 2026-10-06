package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// builtAppFS is a stand-in for a built web/cloud whose index.html links its
// stylesheet the way the bundler writes it.
func builtAppFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {Data: []byte(`<!doctype html><head>` +
			`<script type="module" crossorigin src="/assets/index-abc123.js"></script>` +
			`<link rel="stylesheet" crossorigin href="/assets/index-def456.css">` +
			`</head><body class="datum"><div id="root"></div></body>`)},
		"assets/index-abc123.js":  {Data: []byte("console.log(1)")},
		"assets/index-def456.css": {Data: []byte(".card{padding:24px}")},
	}
}

func TestThePageStylesheetIsTheAppStylesheet(t *testing.T) {
	f := newFixture(t)
	f.srv.spaFS = builtAppFS()

	rec := f.get(t, pageStylesheet)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != ".card{padding:24px}" {
		t.Errorf("body = %q, want the app's stylesheet", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type = %q, want text/css", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
}

func TestThePageStylesheetIsAbsentWithoutTheApp(t *testing.T) {
	for name, fsys := range map[string]fstest.MapFS{
		"not built":     nil,
		"no stylesheet": {"index.html": {Data: []byte("<!doctype html>CLOUD")}},
		"outside assets": {
			"index.html": {Data: []byte(`<link rel="stylesheet" href="/other.css">`)},
			"other.css":  {Data: []byte(".card{}")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if fsys != nil {
				f.srv.spaFS = fsys
			}
			if rec := f.get(t, pageStylesheet); rec.Code != http.StatusNotFound {
				t.Errorf("status %d, want 404: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestServerPagesDrawTheAppCard covers each server-rendered page: the reset
// form, a page() message, and the answer to a console page from before step-up
// moved into the console. Each links the stylesheet, and its policy admits
// styles from this origin and nothing that runs.
func TestServerPagesDrawTheAppCard(t *testing.T) {
	f := newFixture(t)
	f.srv.spaFS = builtAppFS()
	session := f.member(t, "card@example.com", "acme", testConsole, identity.RoleAdmin)

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"stale step-up": f.authorize(t, session, "org=acme&prompt=reauth"),
		"reset":         f.get(t, "/reset?token="+url.QueryEscape("tok")),
		"message":       f.get(t, "/verify?token=nonsense"),
	} {
		t.Run(name, func(t *testing.T) {
			body := rec.Body.String()
			for _, want := range []string{
				`<link rel="stylesheet" href="` + pageStylesheet + `">`,
				`<body class="datum">`,
				`<main class="card">`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("the page lacks %s:\n%s", want, body)
				}
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("Content-Type = %q, want text/html", ct)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
			csp := rec.Header().Get("Content-Security-Policy")
			for _, want := range []string{"default-src 'none'", "style-src 'self'", "font-src 'self'", "img-src 'self'"} {
				if !strings.Contains(csp, want) {
					t.Errorf("the policy lacks %s:\n%s", want, csp)
				}
			}
			for _, banned := range []string{"'unsafe-inline'", "script-src", "*", "data:"} {
				if strings.Contains(csp, banned) {
					t.Errorf("the policy allows %s:\n%s", banned, csp)
				}
			}
		})
	}
}

// A page() message puts its first paragraph in the heading and the rest under
// it.
func TestAMessagePageSplitsItsParagraphs(t *testing.T) {
	f := newFixture(t)

	body := f.get(t, "/verify?token=nonsense").Body.String()
	if !strings.Contains(body, `</svg>This link is not valid any more.</h1>`) {
		t.Errorf("the first paragraph is not the heading:\n%s", body)
	}
	if !strings.Contains(body, `<p class="muted">Verification links last 24 hours`) {
		t.Errorf("the second paragraph is not under the heading:\n%s", body)
	}
}
