package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// The monitor tables.
//
// monitors holds definitions as columns rather than one JSON blob (as
// dashboards do) because the scheduler reads them on every reload and the
// API filters them by name, tag and status; options stay JSON because nothing
// queries inside them and their shape is owned by internal/monitor/spec.
//
// monitor_states is one row per (monitor, group): the whole memory of the
// state machine, so a restart resumes exactly where it stopped.
//
// events is the history: transitions, renotifications, delivery outcomes,
// evaluation errors. It is append-only and pruned by age.
//
// notify_outbox is the reliability seam. A transition's state row, its event
// and the notifications it owes are written in one transaction; delivery
// reads the outbox and deletes a row only once the attempt is over. So a
// notification is never sent for a state that was not persisted, and a crash
// between commit and delivery delays the notification rather than losing it.
const monitorSchema = `
CREATE TABLE IF NOT EXISTS monitors (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	uid          TEXT UNIQUE,
	name         TEXT NOT NULL,
	type         TEXT NOT NULL,
	query        TEXT NOT NULL,
	options_json TEXT NOT NULL DEFAULT '{}',
	message      TEXT NOT NULL DEFAULT '',
	tags_json    TEXT NOT NULL DEFAULT '[]',
	enabled      INTEGER NOT NULL DEFAULT 1,
	muted_until  INTEGER NOT NULL DEFAULT 0,  -- unix seconds; 0 = not muted
	provisioned  INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS monitor_states (
	monitor_id       INTEGER NOT NULL,
	group_key        TEXT NOT NULL,
	group_json       TEXT NOT NULL DEFAULT '{}',
	status           TEXT NOT NULL,
	since_ts         INTEGER NOT NULL,          -- unix ms
	breaches         INTEGER NOT NULL DEFAULT 0,
	last_value       REAL,                      -- NULL before the first value
	last_eval_ts     INTEGER NOT NULL DEFAULT 0,
	last_data_ts     INTEGER NOT NULL DEFAULT 0, -- 0 = never had data
	last_notified_ts INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (monitor_id, group_key)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS events (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	ts          INTEGER NOT NULL,               -- unix ms
	kind        TEXT NOT NULL,
	monitor_id  INTEGER NOT NULL,
	group_key   TEXT NOT NULL DEFAULT '',
	from_status TEXT NOT NULL DEFAULT '',
	to_status   TEXT NOT NULL DEFAULT '',
	value       REAL,
	text        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS events_monitor_ts ON events(monitor_id, ts);
CREATE INDEX IF NOT EXISTS events_ts ON events(ts);
CREATE TABLE IF NOT EXISTS notify_outbox (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	created_ts INTEGER NOT NULL,
	monitor_id INTEGER NOT NULL,
	group_key  TEXT NOT NULL,
	notifier   TEXT NOT NULL,
	payload    TEXT NOT NULL
);
`

// ErrNoMonitor is returned when an id or uid names nothing.
var ErrNoMonitor = errors.New("no such monitor")

// MonitorRow is one stored monitor.
type MonitorRow struct {
	ID          int64
	UID         string
	Name        string
	Type        string
	Query       string
	Options     json.RawMessage
	Message     string
	Tags        []string
	Enabled     bool
	MutedUntil  time.Time // zero: not muted
	Provisioned bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// StateRow is one (monitor, group)'s persisted state. Times are zero when
// unset; LastValue is NaN before the first value.
type StateRow struct {
	MonitorID    int64
	GroupKey     string
	Group        map[string]string
	Status       string
	Since        time.Time
	Breaches     int
	LastValue    float64
	LastEval     time.Time
	LastData     time.Time
	LastNotified time.Time
}

// EventRow is one entry in the history.
type EventRow struct {
	ID        int64
	Ts        time.Time
	Kind      string
	MonitorID int64
	GroupKey  string
	From, To  string
	Value     float64 // NaN: none
	Text      string
}

// OutboxRow is one owed notification: which notifier, and the rendered
// payload it is to deliver (opaque here).
type OutboxRow struct {
	ID        int64
	Created   time.Time
	MonitorID int64
	GroupKey  string
	Notifier  string
	Payload   json.RawMessage
}

// Commit is one evaluation's writes for one monitor, applied atomically.
type Commit struct {
	MonitorID int64
	// Query is the query the evaluation ran. If set and the stored query
	// differs, the commit is refused: the change already reset the groups, and
	// this evaluation's would bring the old query's back.
	Query  string
	Upsert []StateRow
	Delete []string // group keys
	Events []EventRow
	Outbox []OutboxRow
}

const monitorColumns = `SELECT id, COALESCE(uid, ''), name, type, query, options_json, message, tags_json,
	enabled, muted_until, provisioned, created_at, updated_at FROM monitors`

type scanner interface{ Scan(dest ...any) error }

func scanMonitor(s scanner) (MonitorRow, error) {
	var r MonitorRow
	var opts, tags string
	var muted, created, updated int64
	if err := s.Scan(&r.ID, &r.UID, &r.Name, &r.Type, &r.Query, &opts, &r.Message, &tags,
		&r.Enabled, &muted, &r.Provisioned, &created, &updated); err != nil {
		return r, err
	}
	r.Options = json.RawMessage(opts)
	if err := json.Unmarshal([]byte(tags), &r.Tags); err != nil {
		return r, fmt.Errorf("tags_json: %w", err)
	}
	if muted > 0 {
		r.MutedUntil = time.Unix(muted, 0)
	}
	r.CreatedAt, r.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return r, nil
}

// Monitors lists every monitor, by name.
func (d *DB) Monitors(ctx context.Context) ([]MonitorRow, error) {
	rows, err := d.db.QueryContext(ctx, monitorColumns+` ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("meta: listing monitors: %w", err)
	}
	defer rows.Close()
	var out []MonitorRow
	for rows.Next() {
		r, err := scanMonitor(rows)
		if err != nil {
			return nil, fmt.Errorf("meta: listing monitors: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("meta: listing monitors: %w", err)
	}
	return out, nil
}

// Monitor returns one monitor by id.
func (d *DB) Monitor(ctx context.Context, id int64) (MonitorRow, error) {
	return d.oneMonitor(d.db.QueryRowContext(ctx, monitorColumns+` WHERE id = ?`, id), fmt.Sprint(id))
}

// MonitorByUID returns one monitor by its uid.
func (d *DB) MonitorByUID(ctx context.Context, uid string) (MonitorRow, error) {
	return d.oneMonitor(d.db.QueryRowContext(ctx, monitorColumns+` WHERE uid = ?`, uid), uid)
}

func (d *DB) oneMonitor(row *sql.Row, which string) (MonitorRow, error) {
	r, err := scanMonitor(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MonitorRow{}, fmt.Errorf("%w: %s", ErrNoMonitor, which)
	}
	if err != nil {
		return MonitorRow{}, fmt.Errorf("meta: reading monitor %s: %w", which, err)
	}
	return r, nil
}

func tagsJSON(tags []string) string {
	if tags == nil {
		tags = []string{}
	}
	b, _ := json.Marshal(tags)
	return string(b)
}

func optionsText(o json.RawMessage) string {
	if len(o) == 0 {
		return "{}"
	}
	return string(o)
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// CreateMonitor inserts a monitor and returns it as stored.
func (d *DB) CreateMonitor(ctx context.Context, r MonitorRow, now time.Time) (MonitorRow, error) {
	r.CreatedAt = time.Unix(now.Unix(), 0)
	r.UpdatedAt = r.CreatedAt
	res, err := d.db.ExecContext(ctx,
		`INSERT INTO monitors(uid, name, type, query, options_json, message, tags_json, enabled, muted_until,
		 provisioned, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullIfEmpty(r.UID), r.Name, r.Type, r.Query, optionsText(r.Options), r.Message, tagsJSON(r.Tags),
		r.Enabled, unixOrZero(r.MutedUntil), r.Provisioned, r.CreatedAt.Unix(), r.UpdatedAt.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return MonitorRow{}, fmt.Errorf("%w: %q", ErrDuplicateUID, r.UID)
		}
		return MonitorRow{}, fmt.Errorf("meta: creating monitor %q: %w", r.Name, err)
	}
	if r.ID, err = res.LastInsertId(); err != nil {
		return MonitorRow{}, fmt.Errorf("meta: creating monitor %q: %w", r.Name, err)
	}
	return r, nil
}

// UpdateMonitor replaces a monitor's definition. Mute and creation time are
// kept; a provisioned monitor is refused, as for dashboards.
func (d *DB) UpdateMonitor(ctx context.Context, id int64, r MonitorRow, now time.Time) (MonitorRow, error) {
	existing, err := d.Monitor(ctx, id)
	if err != nil {
		return MonitorRow{}, err
	}
	if existing.Provisioned {
		return MonitorRow{}, fmt.Errorf("%w: monitor %d (%s)", ErrProvisioned, id, existing.Name)
	}
	updated := time.Unix(now.Unix(), 0)
	err = d.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE monitors SET uid = ?, name = ?, type = ?, query = ?, options_json = ?, message = ?, tags_json = ?,
			 enabled = ?, updated_at = ? WHERE id = ?`,
			nullIfEmpty(r.UID), r.Name, r.Type, r.Query, optionsText(r.Options), r.Message, tagsJSON(r.Tags),
			r.Enabled, updated.Unix(), id); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: %q", ErrDuplicateUID, r.UID)
			}
			return fmt.Errorf("meta: updating monitor %d: %w", id, err)
		}
		return resetIfQueryChanged(ctx, tx, id, existing.Query, r.Query, now)
	})
	if err != nil {
		return MonitorRow{}, err
	}
	r.ID, r.Provisioned, r.MutedUntil = id, false, existing.MutedUntil
	r.CreatedAt, r.UpdatedAt = existing.CreatedAt, updated
	return r, nil
}

// UpsertProvisionedMonitor writes a monitor read from a file, keyed by uid,
// with the rules UpsertProvisionedDashboard follows: an unchanged file is not
// a write, and a uid owned by an API-created monitor is refused. Mute state
// is never touched — it is operational, not part of the file.
func (d *DB) UpsertProvisionedMonitor(ctx context.Context, r MonitorRow, now time.Time) (MonitorRow, bool, error) {
	if r.UID == "" {
		return MonitorRow{}, false, errors.New("meta: a provisioned monitor needs a uid")
	}
	existing, err := d.MonitorByUID(ctx, r.UID)
	switch {
	case errors.Is(err, ErrNoMonitor):
		r.Provisioned = true
		created, err := d.CreateMonitor(ctx, r, now)
		return created, true, err
	case err != nil:
		return MonitorRow{}, false, err
	}
	if !existing.Provisioned {
		return MonitorRow{}, false, fmt.Errorf(
			"%w: monitor %d (%s) was created through the API, so provisioning will not overwrite it — rename the uid in the file, or delete the monitor",
			ErrDuplicateUID, existing.ID, existing.Name)
	}
	if existing.Name == r.Name && existing.Type == r.Type && existing.Query == r.Query &&
		string(existing.Options) == optionsText(r.Options) && existing.Message == r.Message &&
		tagsJSON(existing.Tags) == tagsJSON(r.Tags) && existing.Enabled == r.Enabled {
		return existing, false, nil
	}
	updated := time.Unix(now.Unix(), 0)
	err = d.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE monitors SET name = ?, type = ?, query = ?, options_json = ?, message = ?, tags_json = ?,
			 enabled = ?, provisioned = 1, updated_at = ? WHERE uid = ?`,
			r.Name, r.Type, r.Query, optionsText(r.Options), r.Message, tagsJSON(r.Tags), r.Enabled,
			updated.Unix(), r.UID); err != nil {
			return fmt.Errorf("meta: provisioning monitor %q: %w", r.UID, err)
		}
		return resetIfQueryChanged(ctx, tx, existing.ID, existing.Query, r.Query, now)
	})
	if err != nil {
		return MonitorRow{}, false, err
	}
	r.ID, r.Provisioned, r.MutedUntil = existing.ID, true, existing.MutedUntil
	r.CreatedAt, r.UpdatedAt = existing.CreatedAt, updated
	return r, true, nil
}

// resetIfQueryChanged deletes a monitor's group states when its query changed,
// in the writer's transaction, and records why.
//
// Here and not only in the scheduler, because the scheduler can only notice a
// change it witnesses: a monitor edited while disabled, or just before a
// restart, comes back as a fresh entry whose persisted groups belong to the old
// query. Stepped as absent they would go NO_DATA and notify for groups that no
// longer exist.
func resetIfQueryChanged(ctx context.Context, tx *sql.Tx, id int64, before, after string, now time.Time) error {
	if before == after {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM monitor_states WHERE monitor_id = ?`, id); err != nil {
		return fmt.Errorf("meta: resetting states of monitor %d: %w", id, err)
	}
	return insertEvent(ctx, tx, EventRow{Ts: now, Kind: "reset", MonitorID: id, Value: math.NaN(),
		Text: "query changed; group states reset"})
}

// ProvisionedMonitors returns the uid and name of every provisioned monitor.
func (d *DB) ProvisionedMonitors(ctx context.Context) (map[string]string, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT uid, name FROM monitors WHERE provisioned = 1`)
	if err != nil {
		return nil, fmt.Errorf("meta: listing provisioned monitors: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var uid, name string
		if err := rows.Scan(&uid, &name); err != nil {
			return nil, fmt.Errorf("meta: listing provisioned monitors: %w", err)
		}
		out[uid] = name
	}
	return out, rows.Err()
}

// ReleaseMonitors turns the provisioned monitors with these uids into
// ordinary, API-managed ones, each with a `released` event.
//
// Released, not deleted: deleting would lose a monitor's history to a file
// that was removed by mistake. But left provisioned, a monitor whose file is
// gone keeps running while the API refuses every edit with "edit the file it
// is provisioned from". Released, it keeps running and can be edited or
// deleted. Deciding *when* a file is gone is the provisioner's job.
func (d *DB) ReleaseMonitors(ctx context.Context, uids []string, now time.Time) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		for _, uid := range uids {
			var id int64
			err := tx.QueryRowContext(ctx, `UPDATE monitors SET provisioned = 0, updated_at = ?
				WHERE uid = ? AND provisioned = 1 RETURNING id`, now.Unix(), uid).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("meta: releasing monitor %q: %w", uid, err)
			}
			if err := insertEvent(ctx, tx, EventRow{Ts: now, Kind: "released", MonitorID: id, Value: math.NaN(),
				Text: "its provisioning file is gone; it is now managed through the API"}); err != nil {
				return err
			}
		}
		return nil
	})
}

// ErrNotRunnable is returned by Commit for a monitor that was deleted,
// disabled or given a new query while its evaluation was in flight.
var ErrNotRunnable = errors.New("monitor was deleted, disabled or changed during its evaluation")

// SetMute mutes a monitor until the given time (zero: unmute). Allowed on a
// provisioned monitor: muting is what an operator does during an incident,
// and a file in git is the wrong place to record it.
func (d *DB) SetMute(ctx context.Context, id int64, until time.Time) (MonitorRow, error) {
	res, err := d.db.ExecContext(ctx, `UPDATE monitors SET muted_until = ? WHERE id = ?`, unixOrZero(until), id)
	if err != nil {
		return MonitorRow{}, fmt.Errorf("meta: muting monitor %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return MonitorRow{}, fmt.Errorf("%w: %d", ErrNoMonitor, id)
	}
	return d.Monitor(ctx, id)
}

// DeleteMonitor removes a monitor with its states, pending notifications and
// events. A provisioned one is refused.
func (d *DB) DeleteMonitor(ctx context.Context, id int64) error {
	existing, err := d.Monitor(ctx, id)
	if err != nil {
		return err
	}
	if existing.Provisioned {
		return fmt.Errorf("%w: monitor %d (%s)", ErrProvisioned, id, existing.Name)
	}
	return d.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM monitor_states WHERE monitor_id = ?`,
			`DELETE FROM notify_outbox WHERE monitor_id = ?`,
			`DELETE FROM events WHERE monitor_id = ?`,
			`DELETE FROM monitors WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return fmt.Errorf("meta: deleting monitor %d: %w", id, err)
			}
		}
		return nil
	})
}

func (d *DB) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("meta: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("meta: commit: %w", err)
	}
	return nil
}

func msOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func timeOrZero(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func nullFloat(v float64) any {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return v
}

// MonitorStatuses is an enabled monitor with its groups' statuses.
type MonitorStatuses struct {
	Row      MonitorRow
	Statuses []string
}

// EnabledStatuses returns each enabled monitor that has states, with their
// statuses: the nav badge's question, in one consistent read. A disabled
// monitor's stored states describe when it last ran, not now. The row comes
// along so the caller can tell whether its definition still compiles.
func (d *DB) EnabledStatuses(ctx context.Context) ([]MonitorStatuses, error) {
	// One row per monitor; statuses are fixed words, so a comma joins them
	// safely.
	rows, err := d.db.QueryContext(ctx, `SELECT m.id, COALESCE(m.uid, ''), m.name, m.type, m.query,
		m.options_json, m.message, m.tags_json, m.enabled, m.muted_until, m.provisioned, m.created_at,
		m.updated_at, GROUP_CONCAT(s.status)
		FROM monitors m JOIN monitor_states s ON s.monitor_id = m.id
		WHERE m.enabled = 1 GROUP BY m.id ORDER BY m.id`)
	if err != nil {
		return nil, fmt.Errorf("meta: reading monitor statuses: %w", err)
	}
	defer rows.Close()
	var out []MonitorStatuses
	for rows.Next() {
		var statuses string
		r, err := scanMonitor(withLast{rows, &statuses})
		if err != nil {
			return nil, fmt.Errorf("meta: reading monitor statuses: %w", err)
		}
		out = append(out, MonitorStatuses{Row: r, Statuses: strings.Split(statuses, ",")})
	}
	return out, rows.Err()
}

// withLast scans one trailing column into last, after the monitor columns.
type withLast struct {
	s    scanner
	last any
}

func (w withLast) Scan(dest ...any) error { return w.s.Scan(append(dest, w.last)...) }

// States returns a monitor's states, or every monitor's when monitorID is 0.
func (d *DB) States(ctx context.Context, monitorID int64) ([]StateRow, error) {
	q := `SELECT monitor_id, group_key, group_json, status, since_ts, breaches, last_value, last_eval_ts,
		last_data_ts, last_notified_ts FROM monitor_states`
	args := []any{}
	if monitorID != 0 {
		q += ` WHERE monitor_id = ?`
		args = append(args, monitorID)
	}
	rows, err := d.db.QueryContext(ctx, q+` ORDER BY monitor_id, group_key`, args...)
	if err != nil {
		return nil, fmt.Errorf("meta: reading monitor states: %w", err)
	}
	defer rows.Close()
	var out []StateRow
	for rows.Next() {
		var r StateRow
		var g string
		var since, eval, data, notified int64
		var last sql.NullFloat64
		if err := rows.Scan(&r.MonitorID, &r.GroupKey, &g, &r.Status, &since, &r.Breaches, &last, &eval,
			&data, &notified); err != nil {
			return nil, fmt.Errorf("meta: reading monitor states: %w", err)
		}
		if err := json.Unmarshal([]byte(g), &r.Group); err != nil {
			return nil, fmt.Errorf("meta: monitor %d group %q: %w", r.MonitorID, r.GroupKey, err)
		}
		r.LastValue = math.NaN()
		if last.Valid {
			r.LastValue = last.Float64
		}
		r.Since, r.LastEval, r.LastData, r.LastNotified = timeOrZero(since), timeOrZero(eval), timeOrZero(data), timeOrZero(notified)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Commit applies one evaluation's state changes, events and owed
// notifications in a single transaction, and returns the outbox rows with
// their assigned ids. Either all of it is durable or none of it is.
func (d *DB) Commit(ctx context.Context, c Commit) ([]OutboxRow, error) {
	out := make([]OutboxRow, len(c.Outbox))
	copy(out, c.Outbox)
	err := d.tx(ctx, func(tx *sql.Tx) error {
		// Inside the transaction, so a delete or disable either happened
		// before this check (nothing is written) or waits for this commit
		// and then removes what it wrote.
		var enabled bool
		var query string
		switch err := tx.QueryRowContext(ctx, `SELECT enabled, query FROM monitors WHERE id = ?`, c.MonitorID).Scan(&enabled, &query); {
		case errors.Is(err, sql.ErrNoRows) || (err == nil && (!enabled || (c.Query != "" && c.Query != query))):
			return ErrNotRunnable
		case err != nil:
			return fmt.Errorf("meta: checking monitor %d: %w", c.MonitorID, err)
		}
		for _, s := range c.Upsert {
			g := s.Group
			if g == nil {
				g = map[string]string{}
			}
			gj, _ := json.Marshal(g)
			if _, err := tx.ExecContext(ctx, `INSERT INTO monitor_states(monitor_id, group_key, group_json, status,
				since_ts, breaches, last_value, last_eval_ts, last_data_ts, last_notified_ts)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(monitor_id, group_key) DO UPDATE SET group_json = excluded.group_json,
				status = excluded.status, since_ts = excluded.since_ts, breaches = excluded.breaches,
				last_value = excluded.last_value, last_eval_ts = excluded.last_eval_ts,
				last_data_ts = excluded.last_data_ts, last_notified_ts = excluded.last_notified_ts`,
				c.MonitorID, s.GroupKey, string(gj), s.Status, msOrZero(s.Since), s.Breaches, nullFloat(s.LastValue),
				msOrZero(s.LastEval), msOrZero(s.LastData), msOrZero(s.LastNotified)); err != nil {
				return fmt.Errorf("meta: writing state %d/%s: %w", c.MonitorID, s.GroupKey, err)
			}
		}
		for _, k := range c.Delete {
			if _, err := tx.ExecContext(ctx, `DELETE FROM monitor_states WHERE monitor_id = ? AND group_key = ?`,
				c.MonitorID, k); err != nil {
				return fmt.Errorf("meta: deleting state %d/%s: %w", c.MonitorID, k, err)
			}
		}
		for _, e := range c.Events {
			if err := insertEvent(ctx, tx, e); err != nil {
				return err
			}
		}
		for i, o := range out {
			res, err := tx.ExecContext(ctx, `INSERT INTO notify_outbox(created_ts, monitor_id, group_key, notifier, payload)
				VALUES(?, ?, ?, ?, ?)`, msOrZero(o.Created), o.MonitorID, o.GroupKey, o.Notifier, string(o.Payload))
			if err != nil {
				return fmt.Errorf("meta: queueing notification: %w", err)
			}
			if out[i].ID, err = res.LastInsertId(); err != nil {
				return fmt.Errorf("meta: queueing notification: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertEvent(ctx context.Context, x execer, e EventRow) error {
	if _, err := x.ExecContext(ctx, `INSERT INTO events(ts, kind, monitor_id, group_key, from_status, to_status, value, text)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, msOrZero(e.Ts), e.Kind, e.MonitorID, e.GroupKey, e.From, e.To,
		nullFloat(e.Value), e.Text); err != nil {
		return fmt.Errorf("meta: writing event: %w", err)
	}
	return nil
}

// AddEvent appends one event outside a commit (an evaluation error, a mute).
func (d *DB) AddEvent(ctx context.Context, e EventRow) error { return insertEvent(ctx, d.db, e) }

// EventQuery selects events. Zero values mean no constraint; Limit defaults
// to 500 and is capped at 5000.
type EventQuery struct {
	MonitorID int64
	From, To  time.Time
	Kinds     []string
	Limit     int
}

// Events returns matching events, newest first.
func (d *DB) Events(ctx context.Context, q EventQuery) ([]EventRow, error) {
	var where []string
	var args []any
	if q.MonitorID != 0 {
		where, args = append(where, "monitor_id = ?"), append(args, q.MonitorID)
	}
	if !q.From.IsZero() {
		where, args = append(where, "ts >= ?"), append(args, q.From.UnixMilli())
	}
	if !q.To.IsZero() {
		where, args = append(where, "ts <= ?"), append(args, q.To.UnixMilli())
	}
	if len(q.Kinds) > 0 {
		where = append(where, "kind IN (?"+strings.Repeat(", ?", len(q.Kinds)-1)+")")
		for _, k := range q.Kinds {
			args = append(args, k)
		}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 500
	}
	limit = min(limit, 5000)
	sqlText := `SELECT id, ts, kind, monitor_id, group_key, from_status, to_status, value, text FROM events`
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := d.db.QueryContext(ctx, sqlText+` ORDER BY ts DESC, id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("meta: reading events: %w", err)
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		var ts int64
		var v sql.NullFloat64
		if err := rows.Scan(&e.ID, &ts, &e.Kind, &e.MonitorID, &e.GroupKey, &e.From, &e.To, &v, &e.Text); err != nil {
			return nil, fmt.Errorf("meta: reading events: %w", err)
		}
		e.Ts, e.Value = time.UnixMilli(ts), math.NaN()
		if v.Valid {
			e.Value = v.Float64
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneEvents deletes events older than before and returns how many.
func (d *DB) PruneEvents(ctx context.Context, before time.Time) (int64, error) {
	res, err := d.db.ExecContext(ctx, `DELETE FROM events WHERE ts < ?`, before.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("meta: pruning events: %w", err)
	}
	return res.RowsAffected()
}

// Outbox returns every undelivered notification, oldest first.
func (d *DB) Outbox(ctx context.Context) ([]OutboxRow, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT id, created_ts, monitor_id, group_key, notifier, payload
		FROM notify_outbox ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("meta: reading outbox: %w", err)
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		var o OutboxRow
		var created int64
		var p string
		if err := rows.Scan(&o.ID, &created, &o.MonitorID, &o.GroupKey, &o.Notifier, &p); err != nil {
			return nil, fmt.Errorf("meta: reading outbox: %w", err)
		}
		o.Created, o.Payload = timeOrZero(created), json.RawMessage(p)
		out = append(out, o)
	}
	return out, rows.Err()
}

// CompleteOutbox removes a delivered (or abandoned) notification and records
// the outcome, atomically: an outcome is never recorded for a notification
// that would be delivered again, and none is lost.
func (d *DB) CompleteOutbox(ctx context.Context, id int64, outcome EventRow) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM notify_outbox WHERE id = ?`, id); err != nil {
			return fmt.Errorf("meta: completing notification %d: %w", id, err)
		}
		return insertEvent(ctx, tx, outcome)
	})
}
