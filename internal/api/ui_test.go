package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var built = fstest.MapFS{
	"index.html":           {Data: []byte("<html>app</html>")},
	"assets/app-abc123.js": {Data: []byte("console.log(1)")},
	"favicon.svg":          {Data: []byte("<svg/>")},
	"assets/sub/.keep":     {Data: nil},
}

func serve(h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestUI_ServesHashedAssetsWithImmutableCache(t *testing.T) {
	rec := serve(UI(built), "/assets/app-abc123.js")
	if rec.Code != 200 || rec.Body.String() != "console.log(1)" {
		t.Fatalf("got %d %q", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable", cc)
	}
}

func TestUI_ServesOtherFilesWithoutLongCache(t *testing.T) {
	rec := serve(UI(built), "/favicon.svg")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "" {
		t.Fatalf("got %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

// Deep links must survive a reload: unknown paths get the app, uncached.
func TestUI_FallsBackToIndexForClientRoutes(t *testing.T) {
	for _, p := range []string{"/", "/index.html", "/dashboards/abc", "/logs?q=x", "/assets/../metrics"} {
		rec := serve(UI(built), p)
		if rec.Code != 200 || rec.Body.String() != "<html>app</html>" {
			t.Errorf("%s: got %d %q, want index.html", p, rec.Code, rec.Body)
		}
		if rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: Cache-Control = %q, want no-cache", p, rec.Header().Get("Cache-Control"))
		}
	}
}

// A stale page asking for an old asset hash must get a 404, not HTML
// masquerading as JavaScript.
func TestUI_MissingAssetIsNotFound(t *testing.T) {
	for _, p := range []string{"/assets/app-old999.js", "/assets/sub"} {
		if rec := serve(UI(built), p); rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404", p, rec.Code)
		}
	}
}

func TestUI_ExplainsHowToBuildWhenUIIsAbsent(t *testing.T) {
	empty := fstest.MapFS{".gitkeep": {Data: nil}}
	for _, p := range []string{"/", "/metrics"} {
		rec := serve(UI(empty), p)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "make web") {
			t.Errorf("%s: got %d %q", p, rec.Code, rec.Body)
		}
	}
}
