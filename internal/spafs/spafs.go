// Package spafs serves a built single-page application from an fs.FS.
//
// Two binaries embed a SPA — the console and Cloud — and both need the same
// three behaviours: serve a real file when the path names one, fall back to
// index.html so the client-side router can handle everything else, and say
// something useful when the app was never built. Two copies of that meant a
// fix in one silently missing from the other, so it lives here.
package spafs

import (
	"io"
	"io/fs"
	"net/http"
	"strings"
)

// assetPrefix is the directory the bundler fills with content-hashed files.
//
// Everything under it carries a hash of its own contents in the name, so it can
// be cached forever: a change produces a different name rather than different
// bytes at the same name. Nothing else can — index.html is rewritten in place
// on every build, and files like favicon.svg keep their name across builds.
const assetPrefix = "assets/"

// Handler serves the SPA in fsys.
//
// notBuilt is what a request gets when there is no application to serve — the
// binary was compiled without the embed tag, or dist was never built. It should
// name the command that fixes it.
func Handler(fsys fs.FS, notBuilt string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fsys == nil {
			http.Error(w, notBuilt, http.StatusNotFound)
			return
		}

		// A real file, served as itself.
		//
		// IsRegular matters, and its absence was a defect in the version this
		// replaces. A directory also stats without error, and handing one to
		// http.FileServerFS makes it redirect to the trailing-slash form — so a
		// client route whose first segment happened to match a directory in
		// dist answered 301 instead of loading the app. Requiring a regular
		// file sends those to index.html with the rest.
		if name := strings.TrimPrefix(r.URL.Path, "/"); name != "" && fs.ValidPath(name) {
			if info, err := fs.Stat(fsys, name); err == nil && info.Mode().IsRegular() {
				serveFile(w, r, fsys, name, cacheControl(name))
				return
			}
		}

		// Everything else is a client route. index.html, and let the router in
		// the page work out what it means.
		//
		// no-cache rather than no-store: the browser may keep it, but must
		// revalidate before using it. index.html names the hashed bundles, so a
		// stale copy points at assets that a deploy has already replaced.
		serveFile(w, r, fsys, "index.html", "no-cache")
	})
}

// cacheControl decides how long a file may be reused.
func cacheControl(name string) string {
	if strings.HasPrefix(name, assetPrefix) {
		return "public, max-age=31536000, immutable"
	}
	// Named files that survive a build — favicon.svg and friends. Cacheable,
	// but only after asking.
	return "no-cache"
}

func serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name, cache string) {
	f, err := fsys.Open(name)
	if err != nil {
		// Reached when index.html itself is missing, which means dist exists
		// but holds nothing usable. Same answer as an unbuilt app, because it
		// is the same situation from the caller's side.
		http.NotFound(w, r)
		return
	}
	defer f.Close() //nolint:errcheck

	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Cache-Control", cache)

	// ServeContent gives conditional requests and byte ranges, and picks the
	// content type from the name's extension — which is why `name` is passed
	// rather than r.URL.Path: a client route like /signin has no extension, but
	// the bytes are still index.html.
	//
	// It needs an io.ReadSeeker. embed.FS provides one; an arbitrary fs.FS need
	// not, and this package takes an arbitrary fs.FS. The version this replaces
	// asserted without the comma-ok form and would have panicked. Falling back
	// to io.Copy loses ranges and conditional requests, which is worth strictly
	// more than a panic.
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, info.ModTime(), rs)
		return
	}
	w.Header().Set("Content-Type", contentType(name))
	_, _ = io.Copy(w, f)
}

// contentType names the type ServeContent would have inferred.
func contentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	case strings.HasSuffix(name, ".woff2"):
		return "font/woff2"
	default:
		return "application/octet-stream"
	}
}
