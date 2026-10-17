package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/monitor"
	"github.com/tuvo1106/ozymandias/internal/monitor/notify"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
	"github.com/tuvo1106/ozymandias/internal/monitor/state"
)

// maxMonitorBytes bounds a monitor definition. A monitor is a query, a few
// options and a message of at most spec.MaxMessageBytes.
const maxMonitorBytes = 64 << 10

// mutedForever is what "mute" without an end stores: a time no evaluation will
// reach, so the mute is a value rather than a second flag to keep in step.
var mutedForever = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// MonitorStore is what the monitor endpoints need from the metadata database.
type MonitorStore interface {
	Monitors(ctx context.Context) ([]meta.MonitorRow, error)
	Monitor(ctx context.Context, id int64) (meta.MonitorRow, error)
	CreateMonitor(ctx context.Context, r meta.MonitorRow, now time.Time) (meta.MonitorRow, error)
	UpdateMonitor(ctx context.Context, id int64, r meta.MonitorRow, now time.Time) (meta.MonitorRow, error)
	DeleteMonitor(ctx context.Context, id int64) error
	SetMute(ctx context.Context, id int64, until time.Time) (meta.MonitorRow, error)
	States(ctx context.Context, monitorID int64) ([]meta.StateRow, error)
	Events(ctx context.Context, q meta.EventQuery) ([]meta.EventRow, error)
	AddEvent(ctx context.Context, e meta.EventRow) error
}

// MonitorEngine is the running scheduler, as the API sees it.
type MonitorEngine interface {
	Reload()
	Health() monitor.Health
	DryRun(ctx context.Context, c *spec.Compiled, now time.Time) ([]monitor.GroupValue, error)
	Backtest(ctx context.Context, c *spec.Compiled, id int64, from, to time.Time) (monitor.Backtest, error)
	TestNotification(ctx context.Context, row meta.MonitorRow) (map[string]error, error)
}

// Monitors serves monitor CRUD, states, events and the editor's helpers.
type Monitors struct {
	Store  MonitorStore
	Engine MonitorEngine
	// Notifiers lists what is configured, for mention checking and for the
	// UI. Names and types only: see notify.Info.
	Notifiers []notify.Info
	Clock     clock.Clock
	Logger    *slog.Logger
}

// Register mounts the endpoints on mux.
func (m *Monitors) Register(mux *http.ServeMux) {
	if m.Clock == nil {
		m.Clock = clock.Real()
	}
	if m.Logger == nil {
		m.Logger = slog.New(slog.DiscardHandler)
	}
	mux.HandleFunc("GET /api/v1/monitors", m.list)
	mux.HandleFunc("POST /api/v1/monitors", m.create)
	mux.HandleFunc("GET /api/v1/monitors/status", m.status)
	mux.HandleFunc("POST /api/v1/monitors/validate", m.validate)
	mux.HandleFunc("POST /api/v1/monitors/backtest", m.backtest)
	mux.HandleFunc("GET /api/v1/monitors/{id}", m.get)
	mux.HandleFunc("PUT /api/v1/monitors/{id}", m.update)
	mux.HandleFunc("DELETE /api/v1/monitors/{id}", m.delete)
	mux.HandleFunc("POST /api/v1/monitors/{id}/mute", m.mute)
	mux.HandleFunc("POST /api/v1/monitors/{id}/unmute", m.unmute)
	mux.HandleFunc("GET /api/v1/monitors/{id}/states", m.states)
	mux.HandleFunc("POST /api/v1/monitors/{id}/test-notification", m.testNotification)
	mux.HandleFunc("GET /api/v1/events", m.events)
	mux.HandleFunc("GET /api/v1/notifiers", m.notifiers)
}

// monitorDef is a monitor on the way in: the spec's fields plus whether it
// runs. Enabled is a pointer so that leaving it out means "yes".
type monitorDef struct {
	spec.Monitor
	Enabled *bool `json:"enabled,omitempty"`
}

// monitorJSON is a monitor on the way out.
type monitorJSON struct {
	ID          int64           `json:"id"`
	UID         string          `json:"uid,omitempty"`
	Name        string          `json:"name"`
	Type        string          `json:"type"`
	Query       string          `json:"query"`
	Options     json.RawMessage `json:"options"`
	Message     string          `json:"message"`
	Tags        []string        `json:"tags"`
	Enabled     bool            `json:"enabled"`
	MutedUntil  *int64          `json:"muted_until"` // unix seconds; null = not muted
	Provisioned bool            `json:"provisioned"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	// Status is the worst status among the monitor's groups, or null when it
	// has none yet — a monitor that has never seen data is not OK, it is
	// unknown, and saying OK would be the UI concluding a fact from nothing.
	Status *string        `json:"status"`
	Groups map[string]int `json:"groups"` // count by status
}

// severity orders statuses for "worst of".
var severity = map[string]int{string(state.OK): 0, string(state.Warn): 1, string(state.NoData): 2, string(state.Alert): 3}

func toMonitorJSON(r meta.MonitorRow, states []meta.StateRow, now time.Time) monitorJSON {
	out := monitorJSON{ID: r.ID, UID: r.UID, Name: r.Name, Type: r.Type, Query: r.Query, Options: r.Options,
		Message: r.Message, Tags: r.Tags, Enabled: r.Enabled, Provisioned: r.Provisioned,
		CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(), Groups: map[string]int{}}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	if len(out.Options) == 0 {
		out.Options = json.RawMessage("{}")
	}
	if !r.MutedUntil.IsZero() && r.MutedUntil.After(now) {
		u := r.MutedUntil.Unix()
		out.MutedUntil = &u
	}
	// A disabled monitor's stored states describe the last time it ran, not
	// now: it has no status, so an ALERT it was disabled in does not stay red
	// in the list forever. (The states are kept, so re-enabling resumes
	// rather than re-announcing; /states still shows them.)
	if !r.Enabled {
		return out
	}
	for _, s := range states {
		out.Groups[s.Status]++
		if out.Status == nil || severity[s.Status] > severity[*out.Status] {
			st := s.Status
			out.Status = &st
		}
	}
	return out
}

func (m *Monitors) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := m.Store.Monitors(r.Context())
	if err != nil {
		m.fail(w, r, "listing monitors", err)
		return
	}
	all, err := m.Store.States(r.Context(), 0)
	if err != nil {
		m.fail(w, r, "listing monitor states", err)
		return
	}
	byMonitor := map[int64][]meta.StateRow{}
	for _, s := range all {
		byMonitor[s.MonitorID] = append(byMonitor[s.MonitorID], s)
	}
	now := m.Clock.Now()
	tag, status, name := q.Get("tag"), q.Get("status"), strings.ToLower(q.Get("q"))
	out := []monitorJSON{}
	for _, row := range rows {
		j := toMonitorJSON(row, byMonitor[row.ID], now)
		switch {
		case tag != "" && !slices.Contains(j.Tags, tag):
			continue
		case status != "" && j.Groups[status] == 0:
			continue
		case name != "" && !strings.Contains(strings.ToLower(j.Name), name):
			continue
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"monitors": out})
}

func (m *Monitors) get(w http.ResponseWriter, r *http.Request) {
	id, ok := monitorID(w, r)
	if !ok {
		return
	}
	row, err := m.Store.Monitor(r.Context(), id)
	if err != nil {
		m.fail(w, r, "reading a monitor", err)
		return
	}
	states, err := m.Store.States(r.Context(), id)
	if err != nil {
		m.fail(w, r, "reading monitor states", err)
		return
	}
	writeJSON(w, http.StatusOK, toMonitorJSON(row, states, m.Clock.Now()))
}

// readDefinition decodes and compiles a body, and checks that every mention
// names a configured notifier.
//
// Mentions are checked here and not in spec.Compile because the notifiers are
// this server's configuration, not part of the definition. At the API the
// person who typed @dicsord-ops is there to fix it; provisioning only warns,
// because skipping a whole monitor over a notifier another environment has
// would leave it with no monitor at all.
func (m *Monitors) readDefinition(w http.ResponseWriter, r *http.Request) (monitorDef, *spec.Compiled, bool) {
	var d monitorDef
	if err := readBodyLimit(w, r, maxMonitorBytes, &d); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return d, nil, false
	}
	c, err := spec.Compile(d.Monitor)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return d, nil, false
	}
	if err := m.checkMentions(d.Message); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return d, nil, false
	}
	return d, c, true
}

func (m *Monitors) checkMentions(message string) error {
	var unknown []string
	for _, name := range spec.Mentions(message) {
		if !slices.ContainsFunc(m.Notifiers, func(i notify.Info) bool { return i.Name == name }) {
			unknown = append(unknown, "@"+name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	known := make([]string, len(m.Notifiers))
	for i, n := range m.Notifiers {
		known[i] = "@" + n.Name
	}
	have := "none are configured (notifiers: in ozyd.yaml)"
	if len(known) > 0 {
		have = "configured: " + strings.Join(known, ", ")
	}
	return fmt.Errorf("message mentions %s, which no notifier is called; %s", strings.Join(unknown, ", "), have)
}

func rowOf(d monitorDef) meta.MonitorRow {
	opts, _ := json.Marshal(d.Options)
	enabled := d.Enabled == nil || *d.Enabled
	return meta.MonitorRow{UID: d.UID, Name: d.Name, Type: string(d.Type), Query: d.Query, Options: opts,
		Message: d.Message, Tags: d.Tags, Enabled: enabled}
}

func (m *Monitors) create(w http.ResponseWriter, r *http.Request) {
	d, _, ok := m.readDefinition(w, r)
	if !ok {
		return
	}
	row, err := m.Store.CreateMonitor(r.Context(), rowOf(d), m.Clock.Now())
	if err != nil {
		m.fail(w, r, "creating a monitor", err)
		return
	}
	m.reload()
	w.Header().Set("Location", "/api/v1/monitors/"+strconv.FormatInt(row.ID, 10))
	writeJSON(w, http.StatusCreated, toMonitorJSON(row, nil, m.Clock.Now()))
}

func (m *Monitors) update(w http.ResponseWriter, r *http.Request) {
	id, ok := monitorID(w, r)
	if !ok {
		return
	}
	d, _, ok := m.readDefinition(w, r)
	if !ok {
		return
	}
	row, err := m.Store.UpdateMonitor(r.Context(), id, rowOf(d), m.Clock.Now())
	if err != nil {
		m.fail(w, r, "updating a monitor", err)
		return
	}
	m.reload()
	states, _ := m.Store.States(r.Context(), id)
	writeJSON(w, http.StatusOK, toMonitorJSON(row, states, m.Clock.Now()))
}

func (m *Monitors) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := monitorID(w, r)
	if !ok {
		return
	}
	if err := m.Store.DeleteMonitor(r.Context(), id); err != nil {
		m.fail(w, r, "deleting a monitor", err)
		return
	}
	m.reload()
	w.WriteHeader(http.StatusNoContent)
}

func (m *Monitors) reload() {
	if m.Engine != nil {
		m.Engine.Reload()
	}
}

func (m *Monitors) mute(w http.ResponseWriter, r *http.Request) {
	id, ok := monitorID(w, r)
	if !ok {
		return
	}
	var body struct {
		Until *int64 `json:"until"` // unix seconds; omitted = until unmuted
	}
	if r.ContentLength != 0 {
		if err := readBody(w, r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	now := m.Clock.Now()
	until := mutedForever
	if body.Until != nil {
		until = time.Unix(*body.Until, 0)
		if !until.After(now) {
			writeError(w, http.StatusBadRequest, fmt.Errorf("until (%d) is not in the future", *body.Until))
			return
		}
	}
	m.setMute(w, r, id, until, "muted")
}

func (m *Monitors) unmute(w http.ResponseWriter, r *http.Request) {
	id, ok := monitorID(w, r)
	if !ok {
		return
	}
	m.setMute(w, r, id, time.Time{}, "unmuted")
}

func (m *Monitors) setMute(w http.ResponseWriter, r *http.Request, id int64, until time.Time, kind string) {
	row, err := m.Store.SetMute(r.Context(), id, until)
	if err != nil {
		m.fail(w, r, "muting a monitor", err)
		return
	}
	text := kind
	if kind == "muted" && !until.Equal(mutedForever) {
		text += " until " + until.UTC().Format(time.RFC3339)
	}
	_ = m.Store.AddEvent(r.Context(), meta.EventRow{Ts: m.Clock.Now(), Kind: kind, MonitorID: id, Value: math.NaN(), Text: text})
	m.reload()
	states, _ := m.Store.States(r.Context(), id)
	writeJSON(w, http.StatusOK, toMonitorJSON(row, states, m.Clock.Now()))
}

// stateJSON is one group's state.
type stateJSON struct {
	Group        string            `json:"group"`
	Tags         map[string]string `json:"tags"`
	Status       string            `json:"status"`
	Since        time.Time         `json:"since"`
	Value        *float64          `json:"value"`
	LastEval     *time.Time        `json:"last_eval"`
	LastData     *time.Time        `json:"last_data"`
	LastNotified *time.Time        `json:"last_notified"`
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func (m *Monitors) states(w http.ResponseWriter, r *http.Request) {
	id, ok := monitorID(w, r)
	if !ok {
		return
	}
	if _, err := m.Store.Monitor(r.Context(), id); err != nil {
		m.fail(w, r, "reading a monitor", err)
		return
	}
	rows, err := m.Store.States(r.Context(), id)
	if err != nil {
		m.fail(w, r, "reading monitor states", err)
		return
	}
	out := make([]stateJSON, 0, len(rows))
	for _, s := range rows {
		tags := s.Group
		if tags == nil {
			tags = map[string]string{}
		}
		out = append(out, stateJSON{Group: s.GroupKey, Tags: tags, Status: s.Status, Since: s.Since.UTC(),
			Value: notify.ValuePtr(s.LastValue), LastEval: optTime(s.LastEval), LastData: optTime(s.LastData),
			LastNotified: optTime(s.LastNotified)})
	}
	// Worst first, then by group: the table a person opens during an incident
	// should lead with what is wrong.
	slices.SortStableFunc(out, func(a, b stateJSON) int {
		if d := severity[b.Status] - severity[a.Status]; d != 0 {
			return d
		}
		return strings.Compare(a.Group, b.Group)
	})
	writeJSON(w, http.StatusOK, map[string]any{"states": out})
}

// eventJSON is one history entry.
type eventJSON struct {
	ID        int64     `json:"id"`
	Ts        time.Time `json:"ts"`
	Kind      string    `json:"kind"`
	MonitorID int64     `json:"monitor_id"`
	Group     string    `json:"group,omitempty"`
	From      string    `json:"from,omitempty"`
	To        string    `json:"to,omitempty"`
	Value     *float64  `json:"value"`
	Text      string    `json:"text,omitempty"`
}

func (m *Monitors) events(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	var errs []error
	q := meta.EventQuery{
		MonitorID: intParam(v.Get("monitor_id"), 0, "monitor_id", &errs),
		Limit:     int(intParam(v.Get("limit"), 500, "limit", &errs)),
	}
	if from := intParam(v.Get("from"), 0, "from", &errs); from > 0 {
		q.From = time.Unix(from, 0)
	}
	if to := intParam(v.Get("to"), 0, "to", &errs); to > 0 {
		q.To = time.Unix(to, 0)
	}
	if k := v.Get("kind"); k != "" {
		q.Kinds = strings.Split(k, ",")
	}
	if len(errs) > 0 {
		writeError(w, http.StatusBadRequest, errors.Join(errs...))
		return
	}
	if q.Limit < 1 || q.Limit > 5000 {
		writeError(w, http.StatusBadRequest, errors.New("limit: want 1 to 5000"))
		return
	}
	rows, err := m.Store.Events(r.Context(), q)
	if err != nil {
		m.fail(w, r, "reading events", err)
		return
	}
	out := make([]eventJSON, 0, len(rows))
	for _, e := range rows {
		out = append(out, eventJSON{ID: e.ID, Ts: e.Ts.UTC(), Kind: e.Kind, MonitorID: e.MonitorID, Group: e.GroupKey,
			From: e.From, To: e.To, Value: notify.ValuePtr(e.Value), Text: e.Text})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

func (m *Monitors) validate(w http.ResponseWriter, r *http.Request) {
	var d monitorDef
	if err := readBodyLimit(w, r, maxMonitorBytes, &d); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	c, err := spec.Compile(d.Monitor)
	if err == nil {
		err = m.checkMentions(d.Message)
	}
	if err != nil {
		// 200 with valid:false, not 400: validating is this endpoint's job,
		// and "your monitor is invalid, here is why" is its successful answer.
		writeJSON(w, http.StatusOK, map[string]any{"valid": false, "errors": strings.Split(err.Error(), "\n")})
		return
	}
	resp := map[string]any{"valid": true, "query": c.Parsed.String(), "grouped": c.Parsed.Grouped(),
		"levels": levelsJSON(c.Levels), "mentions": nonNil(spec.Mentions(c.Message))}
	if m.Engine != nil {
		groups, err := m.Engine.DryRun(r.Context(), c, m.Clock.Now())
		if err != nil {
			resp["eval_error"] = err.Error()
		} else {
			resp["groups"] = groups
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// levelsJSON is the thresholds with every default applied — what the editor
// draws as bands. The warning pair appears only when there is a warning.
func levelsJSON(l spec.Levels) map[string]any {
	out := map[string]any{"op": l.Op, "critical": l.Critical, "critical_recovery": l.CriticalRecovery}
	if l.HasWarning {
		out["warning"], out["warning_recovery"] = l.Warning, l.WarningRecovery
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// backtestTimeout bounds one backtest request.
const backtestTimeout = 30 * time.Second

func (m *Monitors) backtest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Monitor monitorDef `json:"monitor"`
		ID      int64      `json:"id"` // the saved monitor's id, for its phase
		From    int64      `json:"from"`
		To      int64      `json:"to"`
	}
	if err := readBodyLimit(w, r, maxMonitorBytes, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	c, err := spec.Compile(body.Monitor.Monitor)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if m.Engine == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("monitors are not running on this server"))
		return
	}
	now := m.Clock.Now()
	from, to := time.Unix(body.From, 0), time.Unix(body.To, 0)
	if body.To == 0 {
		to = now
	}
	if body.From == 0 {
		from = to.Add(-24 * time.Hour)
	}
	// Its own deadline: a backtest reads up to a week, and the server has no
	// write timeout to stop it.
	ctx, cancel := context.WithTimeout(r.Context(), backtestTimeout)
	defer cancel()
	res, err := m.Engine.Backtest(ctx, c, body.ID, from, to)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (m *Monitors) testNotification(w http.ResponseWriter, r *http.Request) {
	id, ok := monitorID(w, r)
	if !ok {
		return
	}
	row, err := m.Store.Monitor(r.Context(), id)
	if err != nil {
		m.fail(w, r, "reading a monitor", err)
		return
	}
	if m.Engine == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("monitors are not running on this server"))
		return
	}
	res, err := m.Engine.TestNotification(r.Context(), row)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	type outcome struct {
		Notifier string `json:"notifier"`
		OK       bool   `json:"ok"`
		Error    string `json:"error,omitempty"`
	}
	out := []outcome{}
	for name, err := range res {
		o := outcome{Notifier: name, OK: err == nil}
		if err != nil {
			o.Error = err.Error()
		}
		out = append(out, o)
	}
	slices.SortFunc(out, func(a, b outcome) int { return strings.Compare(a.Notifier, b.Notifier) })
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

func (m *Monitors) notifiers(w http.ResponseWriter, _ *http.Request) {
	out := m.Notifiers
	if out == nil {
		out = []notify.Info{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifiers": out})
}

// status is the nav badge and the stall banner in one cheap call.
func (m *Monitors) status(w http.ResponseWriter, r *http.Request) {
	all, err := m.Store.States(r.Context(), 0)
	if err != nil {
		m.fail(w, r, "reading monitor states", err)
		return
	}
	rows, err := m.Store.Monitors(r.Context())
	if err != nil {
		m.fail(w, r, "listing monitors", err)
		return
	}
	enabled := map[int64]bool{}
	for _, row := range rows {
		enabled[row.ID] = row.Enabled
	}
	worst := map[int64]string{}
	for _, s := range all {
		if !enabled[s.MonitorID] {
			continue // disabled, or deleted mid-read: not part of "what is alerting"
		}
		if cur, ok := worst[s.MonitorID]; !ok || severity[s.Status] > severity[cur] {
			worst[s.MonitorID] = s.Status
		}
	}
	counts := map[string]int{}
	for _, s := range worst {
		counts[s]++
	}
	resp := map[string]any{"running": m.Engine != nil, "monitors_by_status": counts}
	if m.Engine != nil {
		h := m.Engine.Health()
		resp["monitors"] = h.Monitors
		resp["stalled"] = h.Stalled
		resp["loading"] = h.Loading
		resp["last_eval_age_seconds"] = math.Round(h.LastEvalAge.Seconds())
	}
	writeJSON(w, http.StatusOK, resp)
}

func monitorID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%q is not a monitor id", raw))
		return 0, false
	}
	return id, true
}

// fail turns a store error into a status, as Dashboards.fail does.
func (m *Monitors) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	switch {
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		writeError(w, statusClientClosedRequest, errors.New("the client closed the request"))
	case errors.Is(err, meta.ErrNoMonitor):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, meta.ErrDuplicateUID):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, meta.ErrProvisioned):
		writeError(w, http.StatusConflict, fmt.Errorf(
			"%w — edit the file it is provisioned from, or the next restart will undo this", err))
	default:
		m.Logger.Error("monitor request failed", "what", what, "err", err)
		writeError(w, http.StatusInternalServerError, errors.New("the request could not be completed"))
	}
}

// MonitorProvisionStore adapts *meta.DB for monitor.Provision.
type MonitorProvisionStore struct{ DB *meta.DB }

// ReleaseOrphanedMonitors implements monitor.ProvisionStore.
func (p MonitorProvisionStore) ReleaseOrphanedMonitors(ctx context.Context, keep []string, now time.Time) ([]string, error) {
	return p.DB.ReleaseOrphanedMonitors(ctx, keep, now)
}

// UpsertProvisionedMonitor implements monitor.ProvisionStore.
func (p MonitorProvisionStore) UpsertProvisionedMonitor(ctx context.Context, row meta.MonitorRow, now time.Time) (bool, error) {
	_, changed, err := p.DB.UpsertProvisionedMonitor(ctx, row, now)
	return changed, err
}
