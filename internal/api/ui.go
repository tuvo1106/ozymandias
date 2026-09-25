package api

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// notBuiltPage is served when the binary was built without the web UI (the
// embed directory held only its placeholder).
const notBuiltPage = `<!doctype html>
<meta charset="utf-8"><title>ozymandias</title>
<body style="font-family: system-ui; max-width: 40rem; margin: 4rem auto; line-height: 1.5">
<h1>ozymandias</h1>
<p>This ozyd binary was built without the web UI.</p>
<p>Run <code>make web &amp;&amp; make build</code>, or use <code>make dev</code> for the
Vite dev server on <a href="http://localhost:9401">:9401</a>.
The API and <a href="/healthz">/healthz</a> work either way.</p>
</body>`

// UI returns the handler for the single-page web UI in fsys (the root of a
// Vite build: index.html plus assets/).
//
//   - An existing file is served as-is; files under /assets/ get a one-year
//     immutable cache (their names contain a content hash).
//   - Any other path gets index.html with no-cache, so client-side routes
//     survive a reload and a new build is picked up on the next load.
//   - If fsys has no index.html, every path gets a page explaining how to
//     build the UI.
func UI(fsys fs.FS) http.Handler {
	index, err := fs.ReadFile(fsys, "index.html")
	built := err == nil
	files := http.FileServerFS(fsys)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !built {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(notBuiltPage))
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "index.html" && fileExists(fsys, name) {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		// A missing file under /assets/ is a real 404 (a stale page asking for
		// an old hash), not a client route: serving index.html as JavaScript
		// would fail confusingly in the browser.
		if strings.HasPrefix(name, "assets/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}

func fileExists(fsys fs.FS, name string) bool {
	fi, err := fs.Stat(fsys, name)
	if errors.Is(err, fs.ErrNotExist) || err != nil {
		return false
	}
	return !fi.IsDir()
}
