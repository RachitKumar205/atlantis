package server

import (
	"html/template"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
)

// pageStylesheet is the stylesheet every server-rendered page links. It
// answers with the sign-in app's own stylesheet, so these pages draw the same
// card as the app.
const pageStylesheet = "/page.css"

// cardPage is a page this server renders in the sign-in app's card.
type cardPage struct {
	Title string

	// Lead holds the paragraphs under the title.
	Lead []string

	// Error is shown as an alert above the form.
	Error string

	Form *cardForm
}

type cardForm struct {
	Action string
	Hidden []cardHidden
	Field  cardField
	Submit string
}

type cardHidden struct{ Name, Value string }

type cardField struct {
	Label, Name, Type, InputMode, AutoComplete, Hint string
}

// The markup of Card, Field and Button in web/cloud/src/components/ui.tsx.
var cardTemplate = template.Must(template.New("card").Parse(`<!doctype html>
<html lang="en" data-theme="light">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · Atlantis Cloud</title>
<link rel="icon" type="image/svg+xml" href="/favicon.svg">
<link rel="stylesheet" href="` + pageStylesheet + `">
</head>
<body class="datum">
<div class="shell">
<div class="shell__art" aria-hidden="true"></div>
<div class="shell__form">
<main class="card">
<h1 class="wordmark"><svg class="wordmark__mark" viewBox="0 0 26 26" fill="none" aria-hidden="true"><circle cx="13" cy="13" r="10" stroke="var(--line-strong)" stroke-width="1.3"/><circle cx="13" cy="13" r="5.5" stroke="var(--ink-2)" stroke-width="1.1"/><circle cx="13" cy="13" r="1.9" fill="var(--accent)"/></svg>{{.Title}}</h1>
{{range .Lead}}<p class="muted">{{.}}</p>
{{end}}{{if .Error}}<p class="notice notice--error" role="alert">{{.Error}}</p>
{{end}}{{with .Form}}<form method="post" action="{{.Action}}">
{{range .Hidden}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">
{{end}}{{with .Field}}<div class="field">
<label class="label" for="f-{{.Name}}">{{.Label}}</label>
<input id="f-{{.Name}}" class="input" name="{{.Name}}"{{if .Type}} type="{{.Type}}"{{end}}{{if .InputMode}} inputmode="{{.InputMode}}"{{end}}{{if .AutoComplete}} autocomplete="{{.AutoComplete}}"{{end}} autofocus required>
{{if .Hint}}<p class="hint">{{.Hint}}</p>
{{end}}</div>
{{end}}<button class="btn" type="submit">{{.Submit}}</button>
</form>
{{end}}</main>
</div>
</div>
</body>
</html>
`))

// writeCard renders p.
//
// Nothing on the page runs. Styles, fonts and the artwork load from this
// origin, which is all the policy allows.
func writeCard(w http.ResponseWriter, code int, p cardPage) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'none'",
		"style-src 'self'",
		"font-src 'self'",
		"img-src 'self'",
		"form-action 'self'",
		"base-uri 'none'",
		"frame-ancestors 'none'",
	}, "; "))
	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_ = cardTemplate.Execute(w, p)
}

// stylesheetLink matches the stylesheet link the bundler writes into
// index.html, whose href is a content-hashed file under /assets/.
var stylesheetLink = regexp.MustCompile(`<link\b[^>]*\brel="stylesheet"[^>]*\bhref="/(assets/[^"/]+\.css)"`)

// handlePageStylesheet serves the sign-in app's stylesheet at pageStylesheet.
// It is 404 when the app is not built, and the pages render unstyled.
func (s *Server) handlePageStylesheet(w http.ResponseWriter, r *http.Request) {
	if s.spaFS == nil {
		http.NotFound(w, r)
		return
	}
	index, err := fs.ReadFile(s.spaFS, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	m := stylesheetLink.FindSubmatch(index)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	// no-cache: the path stays the same across builds and the file behind it
	// does not.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, s.spaFS, string(m[1]))
}
