package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/dashboard"
	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

func dashboardsAPI(t *testing.T) (http.Handler, *meta.DB) {
	t.Helper()
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mux := http.NewServeMux()
	(&Dashboards{Store: db, Clock: testutil.NewFakeClock(now)}).Register(mux)
	return mux, db
}

// send performs a request and returns the recorder plus the decoded body, if
// there is one.
func send(t *testing.T, h http.Handler, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: body %q is not JSON", method, path, rec.Body.String())
		}
	}
	return rec, out
}

const definition = `{"title":"Checkout","description":"the money path",
	"template_vars":[{"name":"env","tag":"env","default":"*"}],
	"widgets":[{"id":"w1","type":"timeseries","title":"req/s",
		"layout":{"x":0,"y":0,"w":6,"h":3},
		"queries":[{"q":"sum:http.request.count{$env} by {route}.as_rate()","display":"line"}]}]}`

func TestDashboards_CreateThenReadItBack(t *testing.T) {
	h, _ := dashboardsAPI(t)

	rec, out := send(t, h, http.MethodPost, "/api/v1/dashboards", definition)
	if rec.Code != 201 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	id, ok := out["id"].(float64)
	if !ok || id == 0 {
		t.Fatalf("no id in %v", out)
	}
	if got := rec.Header().Get("Location"); got != "/api/v1/dashboards/1" {
		t.Errorf("Location %q", got)
	}
	// The definition's fields are inlined next to the metadata, so a client can
	// render the thing it just fetched without unwrapping.
	if out["title"] != "Checkout" {
		t.Errorf("title %v", out["title"])
	}
	if _, ok := out["widgets"].([]any); !ok {
		t.Errorf("widgets missing from %v", out)
	}
	if out["provisioned"] != false {
		t.Errorf("provisioned %v, want false", out["provisioned"])
	}

	rec, got := send(t, h, http.MethodGet, "/api/v1/dashboards/1", "")
	if rec.Code != 200 {
		t.Fatalf("%d %v", rec.Code, got)
	}
	if got["title"] != "Checkout" || got["id"] != id {
		t.Errorf("%v", got)
	}
}

// A dashboard exported from one ozyd and imported into another should not pick
// up a diff on the way through, so the stored bytes come back as written.
func TestDashboards_TheDefinitionIsNotRewritten(t *testing.T) {
	h, db := dashboardsAPI(t)
	if rec, out := send(t, h, http.MethodPost, "/api/v1/dashboards", definition); rec.Code != 201 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	row, err := db.Dashboard(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(row.Definition) != definition {
		t.Errorf("the stored definition was rewritten:\n got: %s\nwant: %s", row.Definition, definition)
	}
}

// The database owns id, provisioned and the timestamps. A definition that
// happens to contain them must not be able to claim them.
func TestDashboards_ADefinitionCannotOverwriteTheMetadata(t *testing.T) {
	h, _ := dashboardsAPI(t)
	sneaky := `{"title":"x","id":999,"provisioned":true,
		"widgets":[{"id":"w","type":"note","layout":{"x":0,"y":0,"w":1,"h":1},"markdown":"hi"}]}`
	// `id` is not a field of a definition, so this is refused before it can be
	// a problem — which is the strongest possible answer.
	rec, out := send(t, h, http.MethodPost, "/api/v1/dashboards", sneaky)
	if rec.Code != 400 {
		t.Fatalf("%d %v, want 400", rec.Code, out)
	}
	if !strings.Contains(out["error"].(string), "unknown field") {
		t.Errorf("error %q", out["error"])
	}
}

func TestDashboards_ListIsSortedAndCounted(t *testing.T) {
	h, _ := dashboardsAPI(t)
	for _, title := range []string{"zeta", "alpha"} {
		body := `{"title":"` + title + `","widgets":[{"id":"w","type":"note",` +
			`"layout":{"x":0,"y":0,"w":1,"h":1},"markdown":"hi"}]}`
		if rec, out := send(t, h, http.MethodPost, "/api/v1/dashboards", body); rec.Code != 201 {
			t.Fatalf("%d %v", rec.Code, out)
		}
	}
	rec, out := send(t, h, http.MethodGet, "/api/v1/dashboards", "")
	if rec.Code != 200 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if out["count"] != 2.0 {
		t.Errorf("count %v", out["count"])
	}
	list := out["dashboards"].([]any)
	if list[0].(map[string]any)["title"] != "alpha" {
		t.Errorf("not sorted by title: %v", list)
	}
}

func TestDashboards_ListIsAnEmptyArrayNotNull(t *testing.T) {
	h, _ := dashboardsAPI(t)
	_, out := send(t, h, http.MethodGet, "/api/v1/dashboards", "")
	list, ok := out["dashboards"].([]any)
	if !ok {
		t.Fatalf("dashboards is %T in %v; a client should not have to handle null", out["dashboards"], out)
	}
	if len(list) != 0 {
		t.Errorf("%v", list)
	}
}

func TestDashboards_Update(t *testing.T) {
	h, _ := dashboardsAPI(t)
	if rec, _ := send(t, h, http.MethodPost, "/api/v1/dashboards", definition); rec.Code != 201 {
		t.Fatal(rec.Code)
	}
	edited := strings.Replace(definition, `"title":"Checkout"`, `"title":"Checkout v2"`, 1)
	rec, out := send(t, h, http.MethodPut, "/api/v1/dashboards/1", edited)
	if rec.Code != 200 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if out["title"] != "Checkout v2" {
		t.Errorf("title %v", out["title"])
	}
	// created_at is when it was made; an edit is not a new dashboard.
	if out["created_at"] != out["updated_at"] {
		// The fake clock does not move, so these stay equal — what is being
		// checked is that created_at was not reset to something else entirely.
		t.Errorf("created_at %v, updated_at %v", out["created_at"], out["updated_at"])
	}
}

// Writing to a provisioned dashboard would be undone at the next restart.
func TestDashboards_AProvisionedDashboardIs409(t *testing.T) {
	h, db := dashboardsAPI(t)
	if _, _, err := db.UpsertProvisionedDashboard(context.Background(), meta.DashboardRow{
		UID: "home", Title: "Home", Definition: []byte(`{"title":"Home"}`),
	}, now); err != nil {
		t.Fatal(err)
	}

	rec, out := send(t, h, http.MethodPut, "/api/v1/dashboards/1", definition)
	if rec.Code != 409 {
		t.Fatalf("PUT gave %d %v, want 409", rec.Code, out)
	}
	if msg := out["error"].(string); !strings.Contains(msg, "edit the file") {
		t.Errorf("error %q, want it to say what to do instead", msg)
	}
	rec, out = send(t, h, http.MethodDelete, "/api/v1/dashboards/1", "")
	if rec.Code != 409 {
		t.Errorf("DELETE gave %d %v, want 409", rec.Code, out)
	}
	// And it is still there, still readable.
	if rec, _ := send(t, h, http.MethodGet, "/api/v1/dashboards/1", ""); rec.Code != 200 {
		t.Errorf("the refused write left it at %d", rec.Code)
	}
}

func TestDashboards_Delete(t *testing.T) {
	h, _ := dashboardsAPI(t)
	if rec, _ := send(t, h, http.MethodPost, "/api/v1/dashboards", definition); rec.Code != 201 {
		t.Fatal(rec.Code)
	}
	rec, _ := send(t, h, http.MethodDelete, "/api/v1/dashboards/1", "")
	if rec.Code != 204 {
		t.Fatalf("%d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 with a body: %q", rec.Body.String())
	}
	if rec, _ := send(t, h, http.MethodGet, "/api/v1/dashboards/1", ""); rec.Code != 404 {
		t.Errorf("after delete, GET gave %d", rec.Code)
	}
}

func TestDashboards_MissingIs404(t *testing.T) {
	h, _ := dashboardsAPI(t)
	for _, tc := range []struct{ method, body string }{
		{http.MethodGet, ""},
		{http.MethodPut, definition},
		{http.MethodDelete, ""},
	} {
		rec, out := send(t, h, tc.method, "/api/v1/dashboards/404", tc.body)
		if rec.Code != 404 {
			t.Errorf("%s gave %d %v, want 404", tc.method, rec.Code, out)
		}
	}
}

func TestDashboards_ABadIdIs400(t *testing.T) {
	h, _ := dashboardsAPI(t)
	for _, id := range []string{"abc", "0", "-1", "1.5", "9999999999999999999999"} {
		rec, out := send(t, h, http.MethodGet, "/api/v1/dashboards/"+id, "")
		if rec.Code != 400 {
			t.Errorf("id %q gave %d %v, want 400", id, rec.Code, out)
		}
	}
	// A trailing slash is the mux's 404, not ours: a Go 1.22 wildcard does not
	// match an empty segment, so this never reaches a handler. Pinned because
	// it is the kind of thing a later routing change would silently alter.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/dashboards/", nil))
	if rec.Code != 404 {
		t.Errorf("a trailing slash gave %d, want the mux's 404", rec.Code)
	}
}

// The definition is validated on the way in, so a dashboard that would show
// twelve widgets of error text never gets stored.
func TestDashboards_AnInvalidDefinitionIs400(t *testing.T) {
	h, _ := dashboardsAPI(t)
	for _, tc := range []struct{ name, body, want string }{
		{"no title", `{"widgets":[]}`, "title is required"},
		{"no widgets", `{"title":"x","widgets":[]}`, "at least one widget"},
		{"a query that does not parse", `{"title":"x","widgets":[{"id":"w","type":"timeseries",
			"layout":{"x":0,"y":0,"w":1,"h":1},"queries":[{"q":"sum:x{a:b by {k}"}]}]}`, "expected"},
		{"an undeclared variable", `{"title":"x","widgets":[{"id":"w","type":"timeseries",
			"layout":{"x":0,"y":0,"w":1,"h":1},"queries":[{"q":"sum:x{$env}"}]}]}`, "$env is not declared"},
		{"off the grid", `{"title":"x","widgets":[{"id":"w","type":"note",
			"layout":{"x":10,"y":0,"w":6,"h":1},"markdown":"hi"}]}`, "past the 12-column grid"},
		{"not json", `{`, "dashboard json"},
		{"empty body", ``, "dashboard json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, out := send(t, h, http.MethodPost, "/api/v1/dashboards", tc.body)
			if rec.Code != 400 {
				t.Fatalf("%d %v, want 400", rec.Code, out)
			}
			if !strings.Contains(out["error"].(string), tc.want) {
				t.Errorf("error %q, want it to mention %q", out["error"], tc.want)
			}
		})
	}
}

func TestDashboards_AnOversizedDefinitionIs400(t *testing.T) {
	h, _ := dashboardsAPI(t)
	big := `{"title":"` + strings.Repeat("t", maxDefinitionBytes+1) + `","widgets":[]}`
	rec, out := send(t, h, http.MethodPost, "/api/v1/dashboards", big)
	if rec.Code != 400 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if !strings.Contains(out["error"].(string), "larger than") {
		t.Errorf("error %q, want it to say the definition was too large", out["error"])
	}
}

// failingStore is every method returning the same store failure.
type failingStore struct{ err error }

func (f failingStore) Dashboards(context.Context) ([]meta.DashboardRow, error) { return nil, f.err }
func (f failingStore) Dashboard(context.Context, int64) (meta.DashboardRow, error) {
	return meta.DashboardRow{}, f.err
}

func (f failingStore) CreateDashboard(context.Context, meta.DashboardRow, time.Time) (meta.DashboardRow, error) {
	return meta.DashboardRow{}, f.err
}

func (f failingStore) UpdateDashboard(context.Context, int64, meta.DashboardRow, time.Time) (meta.DashboardRow, error) {
	return meta.DashboardRow{}, f.err
}
func (f failingStore) DeleteDashboard(context.Context, int64) error { return f.err }

// A store failure is ours: the body says nothing about our tables.
func TestDashboards_AStoreFailureIs500AndSaysNothing(t *testing.T) {
	mux := http.NewServeMux()
	(&Dashboards{
		Store: failingStore{errors.New("sql: no such table: dashboards")},
		Clock: testutil.NewFakeClock(now),
	}).Register(mux)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/dashboards", ""},
		{http.MethodGet, "/api/v1/dashboards/1", ""},
		{http.MethodPost, "/api/v1/dashboards", definition},
		{http.MethodPut, "/api/v1/dashboards/1", definition},
		{http.MethodDelete, "/api/v1/dashboards/1", ""},
	} {
		rec, out := send(t, mux, tc.method, tc.path, tc.body)
		if rec.Code != 500 {
			t.Errorf("%s %s gave %d, want 500", tc.method, tc.path, rec.Code)
			continue
		}
		if msg := out["error"].(string); strings.Contains(msg, "sql") || strings.Contains(msg, "table") {
			t.Errorf("%s %s leaked the store's internals: %q", tc.method, tc.path, msg)
		}
	}
}

// A client that hangs up is not a server error, for the same reason as on the
// query endpoint: the UI does it constantly and a log full of it is useless.
func TestDashboards_AnAbandonedRequestIsNotAServerError(t *testing.T) {
	mux := http.NewServeMux()
	(&Dashboards{
		Store: failingStore{context.Canceled},
		Clock: testutil.NewFakeClock(now),
	}).Register(mux)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/dashboards", nil).WithContext(ctx))
	if rec.Code != statusClientClosedRequest {
		t.Errorf("status %d, want %d", rec.Code, statusClientClosedRequest)
	}
}

// The adapter is the whole price of keeping internal/dashboard free of a
// storage dependency, so it should be exercised rather than assumed.
func TestProvisionStore_Adapts(t *testing.T) {
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	store := ProvisionStore{DB: db}
	row := dashboard.Row{UID: "home", Title: "Home", Definition: []byte(`{"title":"Home"}`)}
	changed, err := store.UpsertProvisionedDashboard(context.Background(), row, now)
	if err != nil || !changed {
		t.Fatalf("changed = %v, err = %v", changed, err)
	}
	again, err := store.UpsertProvisionedDashboard(context.Background(), row, now)
	if err != nil || again {
		t.Fatalf("the second pass reported changed = %v, err = %v", again, err)
	}
	stored, err := db.DashboardByUID(context.Background(), "home")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Provisioned || stored.Title != "Home" {
		t.Errorf("%+v", stored)
	}
}

// A definition that came from a file, which is to say: indented, and ending in
// a newline.
//
// This is the case the whole suite missed. Every fixture above is a single-line
// string, so the merge in storedDashboard.MarshalJSON — which splices the
// definition's bytes between the metadata's braces — never met a trailing
// newline, assumed the last byte was '}', and produced malformed JSON for every
// provisioned dashboard while staying green here. It surfaced as HTTP 200 with
// an empty body, because the encoder wrote the status line before discovering
// the problem.
func TestDashboards_ADefinitionFromAFileRoundTrips(t *testing.T) {
	h, db := dashboardsAPI(t)
	fromFile := "{\n  \"uid\": \"home\",\n  \"title\": \"Home\",\n" +
		"  \"widgets\": [\n    {\"id\": \"w1\", \"type\": \"note\",\n" +
		"     \"layout\": {\"x\": 0, \"y\": 0, \"w\": 12, \"h\": 1},\n" +
		"     \"markdown\": \"hi\"}\n  ]\n}\n"
	if _, _, err := db.UpsertProvisionedDashboard(context.Background(), meta.DashboardRow{
		UID: "home", Title: "Home", Definition: []byte(fromFile),
	}, now); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/api/v1/dashboards", "/api/v1/dashboards/1"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		// The bug was an empty body behind a 200, so assert on the bytes
		// before trusting a decoder.
		if rec.Body.Len() == 0 {
			t.Fatalf("%s: 200 with an empty body", path)
		}
		var out any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: the response is not JSON: %v\n%s", path, err, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/dashboards/1", nil))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["title"] != "Home" || got["uid"] != "home" || got["id"] != 1.0 {
		t.Errorf("%v", got)
	}
	if _, ok := got["widgets"].([]any); !ok {
		t.Errorf("widgets missing from %v", got)
	}
}

// A stored definition that is not a JSON object must not become a 200 with an
// unparseable body. Nothing can write one through the API — validation is in
// the way — but a hand-edited database row should still fail loudly.
func TestDashboards_AnUnusableStoredDefinitionIs500NotAnEmpty200(t *testing.T) {
	h, db := dashboardsAPI(t)
	if _, _, err := db.UpsertProvisionedDashboard(context.Background(), meta.DashboardRow{
		UID: "broken", Title: "Broken", Definition: []byte(`not json at all`),
	}, now); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/dashboards/1", nil))
	if rec.Code != 500 {
		t.Errorf("status %d, want 500", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Error("an empty body: a client cannot tell this from success")
	}
}

// An object with nothing but whitespace in it is an object with no fields. A
// length test calls it non-empty and the merge emits a trailing comma — the
// same mistake as assuming the last byte is '}', which is why this is a table
// rather than one case.
func TestDashboards_AnEmptyObjectBodyDoesNotProduceATrailingComma(t *testing.T) {
	for _, def := range []string{`{}`, `{ }`, "{\n}", "{\n  \n}", "  {}  ", "{}\n"} {
		t.Run(strconv.Quote(def), func(t *testing.T) {
			h, db := dashboardsAPI(t)
			if _, _, err := db.UpsertProvisionedDashboard(context.Background(), meta.DashboardRow{
				UID: "x", Title: "X", Definition: []byte(def),
			}, now); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/dashboards/1", nil))
			if rec.Code != 200 {
				t.Fatalf("%d: %s", rec.Code, rec.Body.String())
			}
			var out map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("the response is not JSON: %v\n%s", err, rec.Body.String())
			}
			if out["id"] != 1.0 {
				t.Errorf("%v", out)
			}
		})
	}
}

// A uid somebody already took is the caller's problem, with their uid in it —
// not a 500 about a constraint. It is reachable by importing a dashboard twice.
func TestDashboards_ADuplicateUIDIs409(t *testing.T) {
	h, _ := dashboardsAPI(t)
	body := `{"uid":"home","title":"Home","widgets":[{"id":"w","type":"note",` +
		`"layout":{"x":0,"y":0,"w":1,"h":1},"markdown":"hi"}]}`

	if rec, out := send(t, h, http.MethodPost, "/api/v1/dashboards", body); rec.Code != 201 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	rec, out := send(t, h, http.MethodPost, "/api/v1/dashboards", body)
	if rec.Code != 409 {
		t.Fatalf("the second import gave %d %v, want 409", rec.Code, out)
	}
	msg := out["error"].(string)
	if !strings.Contains(msg, "home") {
		t.Errorf("error %q does not name the uid", msg)
	}
	if strings.Contains(msg, "constraint") || strings.Contains(msg, "SQL") {
		t.Errorf("error %q leaks the constraint", msg)
	}
}

// The apology has to be JSON too: it is the status a client is most likely to
// parse defensively, and docs/api.md says every response is JSON.
func TestWriteJSON_AMarshalFailureIsStillAJSONResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	// A channel cannot be marshalled.
	writeJSON(rec, http.StatusOK, map[string]any{"bad": make(chan int)})

	if rec.Code != 500 {
		t.Errorf("status %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", cc)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("the body is not JSON: %q", rec.Body.String())
	}
	if out["status"] != "error" {
		t.Errorf("%v", out)
	}
}
