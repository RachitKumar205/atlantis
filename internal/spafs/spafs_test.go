package spafs

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"
)

const notBuilt = "SPA not built yet — run: make build-cloud-spa"

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":              {Data: []byte("<!doctype html>APP")},
		"favicon.svg":             {Data: []byte("<svg/>")},
		"assets/index-abc123.js":  {Data: []byte("console.log(1)")},
		"assets/index-abc123.css": {Data: []byte(".a{}")},
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// A real file is served as itself, not as the app.
func TestAnAssetIsServedDirectly(t *testing.T) {
	rec := get(t, Handler(testFS(), notBuilt), "/assets/index-abc123.js")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if rec.Body.String() != "console.log(1)" {
		t.Errorf("body = %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}
}

// Anything that is not a file is the app, so the client router sees it.
func TestAClientRouteFallsBackToIndex(t *testing.T) {
	h := Handler(testFS(), notBuilt)

	for _, path := range []string{"/", "/signin", "/organisations", "/account/security"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d", path, rec.Code)
			continue
		}
		if rec.Body.String() != "<!doctype html>APP" {
			t.Errorf("%s: body = %q", path, rec.Body.String())
		}
	}
}

// A client route that collides with a directory name still loads the app.
//
// This is the defect the extraction fixes. The version this replaces stat'd the
// path, found a directory, and handed it to http.FileServerFS — which redirects
// a directory to its trailing-slash form. So `/assets` answered 301 instead of
// loading the SPA, and any client route named after a directory in dist was
// unreachable.
func TestADirectoryPathLoadsTheAppRatherThanRedirecting(t *testing.T) {
	rec := get(t, Handler(testFS(), notBuilt), "/assets")

	if rec.Code == http.StatusMovedPermanently || rec.Code == http.StatusTemporaryRedirect {
		t.Fatalf("a directory path redirected (%d to %q) instead of serving the app",
			rec.Code, rec.Header().Get("Location"))
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if rec.Body.String() != "<!doctype html>APP" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// Hashed assets are cacheable forever; nothing else is.
//
// index.html names the hashed bundles, so a stale copy points at files a deploy
// has already replaced — which presents as a blank page rather than as a
// caching problem.
func TestCacheControlDistinguishesHashedAssetsFromEverythingElse(t *testing.T) {
	h := Handler(testFS(), notBuilt)

	cases := []struct{ path, want string }{
		{"/assets/index-abc123.js", "public, max-age=31536000, immutable"},
		{"/assets/index-abc123.css", "public, max-age=31536000, immutable"},
		{"/index.html", "no-cache"},
		{"/favicon.svg", "no-cache"},
		{"/signin", "no-cache"},
	}
	for _, c := range cases {
		if got := get(t, h, c.path).Header().Get("Cache-Control"); got != c.want {
			t.Errorf("%s: Cache-Control = %q, want %q", c.path, got, c.want)
		}
	}
}

// No filesystem at all: the binary was built without the embed tag.
func TestANilFilesystemNamesTheFix(t *testing.T) {
	rec := get(t, Handler(nil, notBuilt), "/")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	if body := rec.Body.String(); body == "" || body[:3] == "404" {
		t.Errorf("the answer does not name the fix: %q", body)
	}
}

// A filesystem with no index.html is the same situation as no filesystem.
func TestAMissingIndexIsNotFound(t *testing.T) {
	rec := get(t, Handler(fstest.MapFS{"assets/x.js": {Data: []byte("x")}}, notBuilt), "/signin")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

// unseekableFS returns files that are not io.ReadSeeker.
//
// embed.FS files are seekable, so the unchecked assertion in the code this
// replaces never panicked in production — but the extracted package takes an
// arbitrary fs.FS, which is exactly what widens the input domain.
type unseekableFS struct{ inner fs.FS }

func (u unseekableFS) Open(name string) (fs.File, error) {
	f, err := u.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return unseekableFile{f}, nil
}

type unseekableFile struct{ fs.File }

func (unseekableFile) Read(p []byte) (int, error) { return 0, io.EOF }

func TestANonSeekableFilesystemDoesNotPanic(t *testing.T) {
	// The bug being guarded is a panic, so reaching the assertion at all is the
	// test. A t.Fatal on panic would be redundant — the test binary reports it.
	rec := get(t, Handler(unseekableFS{testFS()}, notBuilt), "/signin")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content-type = %q, want html", ct)
	}
}

// Conditional requests work, which is what ServeContent buys over io.Copy.
func TestAConditionalRequestIsAnsweredNotModified(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html>APP"), ModTime: time.Unix(1_700_000_000, 0)},
	}
	h := Handler(fsys, notBuilt)

	first := get(t, h, "/")
	lastMod := first.Header().Get("Last-Modified")
	if lastMod == "" {
		t.Skip("no Last-Modified to revalidate against")
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("If-Modified-Since", lastMod)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusNotModified {
		t.Errorf("status %d, want 304", rec.Code)
	}
}
