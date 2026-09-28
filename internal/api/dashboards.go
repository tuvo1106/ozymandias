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
}

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

// MarshalJSON splices the definition's fields alongside the metadata, so the
// response is one flat object.
//
// Hand-written rather than an embedded struct, because the definition is stored
// as bytes and never decoded here — see the type comment on [Dashboards]. The
// two objects are merged textually: the metadata first, so a definition that
// somehow contained an "id" cannot overwrite the database's.
func (s storedDashboard) MarshalJSON() ([]byte, error) {
	type metadata struct {
		ID          int64     `json:"id"`
		Provisioned bool      `json:"provisioned"`
		CreatedAt   time.Time `json:"created_at"`
		UpdatedAt   time.Time `json:"updated_at"`
	}
	head, err := json.Marshal(metadata{s.ID, s.Provisioned, s.CreatedAt, s.UpdatedAt})
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
	// {"id":…,"updated_at":…} + {"title":…} -> {"id":…,"updated_at":…,"title":…}
	//
	// The body is trimmed before being tested for emptiness, not measured with
	// len(def) > 2. `{ }` and `{\n}` are objects with no fields, and a length
	// test calls them non-empty and emits a trailing comma — which is the same
	// mistake as the one above, a byte test standing in for a semantic one.
	body := bytes.TrimSpace(def[1 : len(def)-1])
	merged := make([]byte, 0, len(head)+len(body)+1)
	merged = append(merged, head[:len(head)-1]...)
	if len(body) > 0 {
		merged = append(merged, ',')
		merged = append(merged, body...)
	}
	return append(merged, '}'), nil
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
	out := make([]storedDashboard, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToWire(row))
	}
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
		// A count, because a client that paginates later will want it and
		// adding it afterwards is a breaking change for anyone who counted the
		// array themselves.
		Count      int               `json:"count"`
		Dashboards []storedDashboard `json:"dashboards"`
	}{"ok", len(out), out})
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
	writeJSON(w, http.StatusOK, rowToWire(row))
}

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
	writeJSON(w, http.StatusCreated, rowToWire(row))
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
	writeJSON(w, http.StatusOK, rowToWire(row))
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
