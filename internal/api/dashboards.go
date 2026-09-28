package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/dashboard"
	"github.com/tuvo1106/ozymandias/internal/meta"
)

// maxDefinitionBytes bounds a stored definition. A dashboard is a document
// somebody wrote, not an upload: 100 widgets of queries and markdown fits in a
// few tens of kilobytes, and the limit exists so that one POST cannot turn the
// metadata database into a blob store.
const maxDefinitionBytes = 1 << 20

// DashboardStore is what the dashboard endpoints need from the metadata
// database. An interface rather than *meta.DB so that a handler test does not
// need a SQLite file to prove that a 404 is a 404.
type DashboardStore interface {
	Dashboards(ctx context.Context) ([]meta.DashboardRow, error)
	Dashboard(ctx context.Context, id int64) (meta.DashboardRow, error)
	CreateDashboard(ctx context.Context, row meta.DashboardRow, now time.Time) (meta.DashboardRow, error)
	UpdateDashboard(ctx context.Context, id int64, row meta.DashboardRow, now time.Time) (meta.DashboardRow, error)
	DeleteDashboard(ctx context.Context, id int64) error
}

// Dashboards serves dashboard CRUD.
//
// Definitions are validated on the way in and returned verbatim on the way
// out. "Verbatim" is deliberate: the stored bytes are what the author wrote, so
// a GET after a PUT gives back the same JSON rather than this build's
// re-encoding of it — which keeps a dashboard exported from one ozyd and
// imported into another from acquiring spurious diffs.
type Dashboards struct {
	Store  DashboardStore
	Clock  clock.Clock  // default clock.Real()
	Logger *slog.Logger // nil discards

	// Values discovers the services a template dashboard is instantiated for.
	// Nil, like Types, means the two template endpoints answer 503 rather than
	// answering "no services" — see [Dashboards.ready].
	Values TagValueReader
	// Types tells a distribution from a gauge, because a distribution's tags
	// live on its `.count` series and that is where service discovery has to
	// look for them.
	Types MetricTypes

	// mu guards reported, which remembers why each unusable row was unusable so
	// that a broken one is logged once rather than once per request — see
	// [Dashboards.logOnce].
	mu       sync.Mutex
	reported map[report]string
}

// report identifies one complaint about one row: which row, and which condition
// was found wrong with it.
//
// The condition is part of the key because the conditions are independent. A
// definition that does not parse is still spliceable — the CRUD endpoints serve
// the stored bytes back without interpreting them — so one row can be perfectly
// serveable by `GET /api/v1/dashboards` and unusable as a template. Keyed on the
// id alone, a client polling both would clear one condition's record by
// succeeding at the other, and the "say it once" this exists for would become
// "say it every other request".
type report struct {
	id   int64
	what string
}

// The conditions a [report] can be about.
const (
	reportEncode   = "encode"
	reportTemplate = "template"
)

// Register mounts the endpoints on mux.
func (d *Dashboards) Register(mux *http.ServeMux) {
	if d.Clock == nil {
		d.Clock = clock.Real()
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	mux.HandleFunc("GET /api/v1/dashboards", d.list)
	mux.HandleFunc("POST /api/v1/dashboards", d.create)
	mux.HandleFunc("GET /api/v1/dashboards/{id}", d.get)
	mux.HandleFunc("PUT /api/v1/dashboards/{id}", d.update)
	mux.HandleFunc("DELETE /api/v1/dashboards/{id}", d.delete)
	// Both of these are literal segments where {id} is a wildcard, so the
	// router prefers them: Go 1.22 patterns are ordered by specificity, not by
	// registration. `.../services` would otherwise be a dashboard id of
	// "services", which is a 400.
	mux.HandleFunc("GET /api/v1/dashboards/services", d.services)
	mux.HandleFunc("GET /api/v1/dashboards/service/{name}", d.serviceDashboards)
}

// storedDashboard is one dashboard on the wire: the database's columns, then
// the author's definition inlined rather than nested.
//
// Inlined because a client that has just fetched a dashboard wants to render
// it, and `{"id":1,"definition":{…}}` makes every widget access go through a
// wrapper for no gain. json.RawMessage keeps the author's bytes intact through
// the round trip.
type storedDashboard struct {
	ID          int64     `json:"id"`
	Provisioned bool      `json:"provisioned"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Definition is the stored JSON, spliced in by MarshalJSON below.
	Definition json.RawMessage `json:"-"`
}

// dashboardMetadata is the database's half of a stored dashboard: the columns the
// response adds to the author's definition.
//
// A named type at package scope rather than one declared inside MarshalJSON, so
// that a test can compare its field names against [reservedFields] — the list a
// definition is refused for claiming. Two copies of "which keys belong to the
// database" that can drift apart is how that defence becomes imaginary again.
type dashboardMetadata struct {
	ID          int64     `json:"id"`
	Provisioned bool      `json:"provisioned"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// reservedFields are the keys [dashboardMetadata] contributes to a response. A
// stored definition containing any of them is refused rather than served — see
// [storedDashboard.MarshalJSON].
var reservedFields = []string{"id", "provisioned", "created_at", "updated_at"}

// MarshalJSON splices the definition's fields alongside the metadata, so the
// response is one flat object.
//
// Hand-written rather than an embedded struct, because the definition is spliced
// as bytes rather than decoded and re-encoded. What that buys is narrower than an
// earlier version of this comment claimed, so, precisely: the author's key
// *order* survives, which a map would sort, and so does every number exactly as
// written, which a map would turn into a float64 — 12345678901234567890 comes
// back itself rather than as 1.2345678901234567e+19. What does *not* survive is
// whitespace and HTML escaping: encoding/json runs a custom MarshalJSON's bytes
// through compact and escapes <, > and & inside strings, so a pretty-printed
// definition is served minified with "a <b>" as "a \u003cb\u003e" whatever this
// method does. The *stored* bytes are untouched either way — the compaction
// belongs to the response, not to the database.
//
// # The metadata cannot be overridden, and the result is checked
//
// Both of those are scars. Splicing bytes means reasoning about bytes, and this
// function got that wrong three times:
//
//   - It assumed the definition's last byte was '}'. A definition read from a
//     file ends with a newline, so every provisioned dashboard served malformed
//     JSON — behind an HTTP 200, because the encoder had already written the
//     status line. Every test passed, since every fixture was a single-line
//     string.
//   - It used len(def) > 2 to mean "has fields". `{ }` and `{\n}` have none, so
//     the merge emitted a trailing comma.
//   - It wrote the metadata *first*, with a comment claiming that this stopped a
//     definition containing an "id" from overwriting the database's. That is
//     backwards: given two keys of one name, the parsers we have keep the last,
//     so metadata-first meant the definition won. The defence was imaginary.
//
// The first two are fixed by trimming and by testing the trimmed body. The third
// took two attempts. Ordering — the definition first, the metadata last — is not
// a fix on its own, because "the last duplicate key wins" is what parsers happen
// to do and not something JSON promises: Go's own encoding/json/v2 rejects
// duplicate object names outright, so for such a client the row would be a parse
// error rather than a win for the database. A response whose meaning depends on
// whose parser reads it is not an answer. So a definition carrying one of the
// four [reservedFields] is *refused* here, and the ordering stays behind it as a
// second line of defence.
//
// Refused rather than served with those keys stripped, which would keep the
// dashboard renderable: stripping means locating and cutting a member out of raw
// bytes, and the list above is this function's record at byte surgery. Such a row
// cannot be created through the API or by provisioning — dashboard.Parse refuses
// unknown fields on both paths — so it is a hand-edited database, and the useful
// answer to one is which row and why. It costs a decode of the definition on the
// way out, into map[string]json.RawMessage so that only the top level is
// interpreted and nothing is re-encoded. That is the price of the guarantee being
// a guarantee.
//
// # What catches a malformed definition
//
// The decode above, now, and the encoder afterwards. There used to be a third
// thing: the merged bytes were re-checked with json.Valid, under a comment that
// admitted encoding/json validates a custom MarshalJSON's output anyway — it runs
// the bytes through compact and reports a MarshalerError — and then claimed that
// removing the line left every test in the package passing. That was false; a
// test asserted the error came from *this* method, and it failed without it.
// Checking fixed the sentence, and the line is unreachable now regardless: a
// definition that is not a valid object is rejected by reservedField, and a valid
// object's members followed by the metadata's members are a valid object. An
// unreachable guard defended by an untrue sentence is the imaginary defence in
// the third bullet above wearing a different hat, so it is gone.
//
// A splicing bug introduced *below* this comment would therefore be caught where
// the last one was: by the encoder, at the caller, as a 500 rather than an empty
// 200 — and the error names the dashboard, which reaches an operator only because
// the handlers marshal through writeJSONErr and log the id. A review caught that
// being claimed while writeJSON dropped the message on the floor.
//
// The guarantee lives in the tests either way.
func (s storedDashboard) MarshalJSON() ([]byte, error) {
	head, err := json.Marshal(dashboardMetadata{s.ID, s.Provisioned, s.CreatedAt, s.UpdatedAt})
	if err != nil {
		return nil, err
	}
	// Trimmed before anything indexes into it. A definition read from a file
	// ends with a newline, and an earlier version of this code assumed the
	// last byte was '}' — which produced malformed JSON for every provisioned
	// dashboard and valid JSON for every test fixture, because the fixtures
	// were single-line strings. Hence the explicit guard below rather than
	// trust.
	def := bytes.TrimSpace(s.Definition)
	if len(def) == 0 {
		def = []byte("{}")
	}
	if def[0] != '{' || def[len(def)-1] != '}' {
		return nil, fmt.Errorf("dashboard %d: the stored definition is not a JSON object", s.ID)
	}
	// Refused, not overridden: a response cannot carry the same key twice and
	// still mean one thing to every client. See the method comment.
	name, err := s.reservedField(def)
	if err != nil {
		return nil, err
	}
	if name != "" {
		return nil, fmt.Errorf(
			"dashboard %d: the stored definition contains %q, which the database owns — fix the row",
			s.ID, name)
	}
	// {"title":…} + {"id":…,"updated_at":…} -> {"title":…,"id":…,"updated_at":…}
	//
	// The body is trimmed before being tested for emptiness, not measured with
	// len(def) > 2. `{ }` and `{\n}` are objects with no fields, and a length
	// test calls them non-empty and emits a trailing comma — which is the same
	// mistake as assuming the last byte, a byte test standing in for a semantic
	// one.
	body := bytes.TrimSpace(def[1 : len(def)-1])
	merged := make([]byte, 0, len(head)+len(body)+1)
	merged = append(merged, '{')
	if len(body) > 0 {
		merged = append(merged, body...)
		merged = append(merged, ',')
	}
	// head is `{"id":…}`; everything after its opening brace, including the
	// closing one, completes the object.
	merged = append(merged, head[1:]...)
	return merged, nil
}

// reservedField reports the first of [reservedFields] the definition claims, or
// "" if it claims none.
//
// The values are decoded as json.RawMessage so that only the top level is
// interpreted: what gets spliced is still the definition's own bytes, and a
// number buried in a widget is never turned into a float64 and back.
func (s storedDashboard) reservedField(def []byte) (string, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(def, &top); err != nil {
		return "", fmt.Errorf("dashboard %d: the stored definition is not valid JSON: %w", s.ID, err)
	}
	for _, name := range reservedFields {
		if _, ok := top[name]; ok {
			return name, nil
		}
	}
	return "", nil
}

func rowToWire(r meta.DashboardRow) storedDashboard {
	return storedDashboard{
		ID:          r.ID,
		Provisioned: r.Provisioned,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
		Definition:  json.RawMessage(r.Definition),
	}
}

func (d *Dashboards) list(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Store.Dashboards(r.Context())
	if err != nil {
		d.fail(w, r, "listing dashboards", err)
		return
	}
	// Marshalled a row at a time, so that one unusable definition costs its own
	// entry rather than the whole list.
	//
	// Encoding the slice in one call would make a single bad row a 500 for every
	// dashboard — and the list is what the picker is built on, so the blast
	// radius of one hand-edited row would be "nobody can open anything". A
	// dashboard that cannot be rendered is still worth naming, so its id goes in
	// `unreadable` rather than vanishing.
	out := make([]json.RawMessage, 0, len(rows))
	unreadable := []int64{}
	for _, row := range rows {
		body, err := json.Marshal(rowToWire(row))
		if err != nil {
			d.logUnreadable(row, err, "a stored dashboard could not be encoded; it is omitted from the list")
			unreadable = append(unreadable, row.ID)
			continue
		}
		d.encoded(row.ID)
		out = append(out, body)
	}
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
		// A count, because a client that paginates later will want it and
		// adding it afterwards is a breaking change for anyone who counted the
		// array themselves. It counts what is in `dashboards`, not what is in
		// the database — see `unreadable`.
		Count      int               `json:"count"`
		Dashboards []json.RawMessage `json:"dashboards"`
		// Unreadable names the rows whose definitions could not be spliced.
		// Always present: a client should be able to tell "nothing is wrong"
		// from "this field does not exist in your version".
		Unreadable []int64 `json:"unreadable"`
	}{"ok", len(out), out, unreadable})
}

func (d *Dashboards) get(w http.ResponseWriter, r *http.Request) {
	id, ok := d.pathID(w, r)
	if !ok {
		return
	}
	row, err := d.Store.Dashboard(r.Context(), id)
	if err != nil {
		d.fail(w, r, "reading a dashboard", err)
		return
	}
	d.writeDashboard(w, http.StatusOK, row)
}

// writeDashboard answers with one stored dashboard, logging the id if its
// definition cannot be spliced.
//
// The logging is the point. A row that will not marshal is a row somebody has to
// go and fix, and the response deliberately says nothing about it, so the log is
// the only place the id can appear.
func (d *Dashboards) writeDashboard(w http.ResponseWriter, code int, row meta.DashboardRow) {
	if err := writeJSONErr(w, code, rowToWire(row)); err != nil {
		d.logUnreadable(row, err, "a stored dashboard could not be encoded")
		return
	}
	d.encoded(row.ID)
}

// logUnreadable reports a row whose definition could not be encoded: once per
// row, and again only if the reason changes.
//
// Not on every request, which is what it used to be. The list endpoint is what a
// dashboard picker polls, and a hand-edited row stays broken until somebody fixes
// the database — so an Error record per poll is an unbounded stream of identical
// lines, burying everything else in the log for a condition that is not going to
// change. The durable signal is `unreadable` in the response; the log's job is to
// say *why*, and it only has to say it once.
//
// Keyed on the reason as well as the id, so that a row edited into a different
// kind of broken is still heard, and cleared by [Dashboards.encoded] when a row
// comes back, so that a row which breaks twice is reported twice.
func (d *Dashboards) logUnreadable(row meta.DashboardRow, err error, msg string) {
	d.logOnce(report{row.ID, reportEncode}, row, err, msg)
}

// logDropped reports a row that could not be used as a template, under the same
// rule and for the same reasons as [Dashboards.logUnreadable]: the durable
// signal is `unreadable` in the response, and the log's job is to say why once.
func (d *Dashboards) logDropped(row meta.DashboardRow, err error, msg string) {
	d.logOnce(report{row.ID, reportTemplate}, row, err, msg)
}

func (d *Dashboards) logOnce(key report, row meta.DashboardRow, err error, msg string) {
	reason := err.Error()
	d.mu.Lock()
	last, seen := d.reported[key]
	if d.reported == nil {
		d.reported = make(map[report]string)
	}
	d.reported[key] = reason
	d.mu.Unlock()
	if seen && last == reason {
		return
	}
	d.Logger.Error(msg, "id", row.ID, "uid", row.UID, "err", err)
}

// forget drops a row's record of one condition, so that a row which breaks,
// gets fixed and breaks again the same way is reported both times.
func (d *Dashboards) forget(key report) {
	d.mu.Lock()
	delete(d.reported, key)
	d.mu.Unlock()
}

// encoded forgets a row that marshalled cleanly.
func (d *Dashboards) encoded(id int64) { d.forget(report{id, reportEncode}) }

func (d *Dashboards) create(w http.ResponseWriter, r *http.Request) {
	data, def, ok := d.readDefinition(w, r)
	if !ok {
		return
	}
	row, err := d.Store.CreateDashboard(r.Context(), meta.DashboardRow{
		UID:         def.UID,
		Title:       def.Title,
		Description: def.Description,
		Definition:  data,
	}, d.Clock.Now())
	if err != nil {
		d.fail(w, r, "creating a dashboard", err)
		return
	}
	// 201 with a Location header: a client that just created a dashboard needs
	// its URL, and the id is not something it could have known.
	w.Header().Set("Location", "/api/v1/dashboards/"+strconv.FormatInt(row.ID, 10))
	d.writeDashboard(w, http.StatusCreated, row)
}

func (d *Dashboards) update(w http.ResponseWriter, r *http.Request) {
	id, ok := d.pathID(w, r)
	if !ok {
		return
	}
	data, def, ok := d.readDefinition(w, r)
	if !ok {
		return
	}
	row, err := d.Store.UpdateDashboard(r.Context(), id, meta.DashboardRow{
		UID:         def.UID,
		Title:       def.Title,
		Description: def.Description,
		Definition:  data,
	}, d.Clock.Now())
	if err != nil {
		d.fail(w, r, "updating a dashboard", err)
		return
	}
	d.writeDashboard(w, http.StatusOK, row)
}

func (d *Dashboards) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := d.pathID(w, r)
	if !ok {
		return
	}
	if err := d.Store.DeleteDashboard(r.Context(), id); err != nil {
		d.fail(w, r, "deleting a dashboard", err)
		return
	}
	// 204: there is nothing left to describe, and a body saying so is a body
	// every client has to decide whether to parse.
	w.WriteHeader(http.StatusNoContent)
}

// readDefinition reads, parses and validates a definition from the body,
// returning the original bytes as well as the parsed form.
//
// Both, because the two have different jobs: the parsed definition supplies the
// columns and proves the thing is usable, and the original bytes are what gets
// stored so that a round trip does not rewrite the author's JSON.
func (d *Dashboards) readDefinition(w http.ResponseWriter, r *http.Request) ([]byte, dashboard.Dashboard, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDefinitionBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusBadRequest,
				fmt.Errorf("a dashboard definition is larger than %d bytes", maxDefinitionBytes))
			return nil, dashboard.Dashboard{}, false
		}
		writeError(w, http.StatusBadRequest, fmt.Errorf("reading the body: %w", err))
		return nil, dashboard.Dashboard{}, false
	}
	def, err := dashboard.Parse(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return nil, dashboard.Dashboard{}, false
	}
	return data, def, true
}

// pathID reads the {id} wildcard.
func (d *Dashboards) pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%q is not a dashboard id", raw))
		return 0, false
	}
	return id, true
}

// fail turns a store error into a status.
//
// The three that are the caller's get their own code and keep their message:
// nothing there (404), a definition the caller may not write (409), and a
// definition that does not validate (400, raised before we get here). Anything
// else is ours, so it is logged and answered with nothing useful — the same
// rule the query endpoint follows, for the same reason.
func (d *Dashboards) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	switch {
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		d.Logger.Debug("dashboard request abandoned by the client", "what", what)
		writeError(w, statusClientClosedRequest, errors.New("the client closed the request"))
	case errors.Is(err, meta.ErrNoDashboard):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, meta.ErrDuplicateUID):
		// 409: the uid is the caller's to choose and somebody already chose it.
		// This is reachable on the import path docs/api.md advertises, so it
		// keeps its message — which names the uid.
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, meta.ErrProvisioned):
		// 409 rather than 403: this is not about who the caller is, it is that
		// the resource's state makes the write meaningless. The message says
		// what to do instead — edit the file.
		writeError(w, http.StatusConflict, fmt.Errorf(
			"%w — edit the file it is provisioned from, or the next restart will undo this", err))
	case errors.Is(err, dashboard.ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	default:
		d.Logger.Error("dashboard request failed", "what", what, "err", err)
		writeError(w, http.StatusInternalServerError, errors.New("the request could not be completed"))
	}
}

// ProvisionStore adapts *meta.DB to the narrow interface
// [dashboard.Provision] asks for.
//
// The adapter exists so that internal/dashboard does not import internal/meta:
// a definition is a domain concept and should not know what database is under
// it. Six lines here is the whole price of that.
type ProvisionStore struct{ DB *meta.DB }

// UpsertProvisionedDashboard implements dashboard.Store.
func (p ProvisionStore) UpsertProvisionedDashboard(ctx context.Context, row dashboard.Row, now time.Time) (bool, error) {
	_, changed, err := p.DB.UpsertProvisionedDashboard(ctx, meta.DashboardRow{
		UID:         row.UID,
		Title:       row.Title,
		Description: row.Description,
		Definition:  row.Definition,
	}, now)
	return changed, err
}
