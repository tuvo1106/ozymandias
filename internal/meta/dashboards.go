package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The dashboards table.
//
// The definition is stored as opaque JSON text, not as columns. Two reasons,
// and the second is the one that decided it:
//
//   - A dashboard is edited as a whole. Nobody asks "which widgets are 6
//     columns wide"; every read wants the definition and every write replaces
//     it, so normalising widgets into rows would buy a join and a migration
//     for each field the UI grows and sell nothing.
//   - The schema of a definition is versioned by the code that parses it
//     (internal/dashboard), and that is where a compatibility decision
//     belongs. Spreading it across SQLite columns would mean every added
//     widget option is a schema migration on a database somebody is running.
//
// The columns that *are* columns are the ones the database has to sort, filter
// or enforce on: uid for upsert identity, title for listing, provisioned so
// the API can refuse to write to a file-managed dashboard, and the timestamps.
const dashboardSchema = `
CREATE TABLE IF NOT EXISTS dashboards (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	uid         TEXT UNIQUE,       -- NULL for API-created; set for provisioned
	title       TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	definition  TEXT NOT NULL,     -- the whole definition, as JSON
	provisioned INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL,  -- unix seconds
	updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS dashboards_title ON dashboards(title);
`

// ErrNoDashboard is returned when an id or uid names nothing.
var ErrNoDashboard = errors.New("no such dashboard")

// ErrProvisioned is returned when a write targets a dashboard that came from a
// file.
//
// Allowing the write would be worse than refusing it: provisioning runs at
// every startup and would silently restore the file's version, so the edit
// would vanish at the next restart with nothing anywhere explaining why. The
// person to tell is the one making the edit, at the moment they make it.
var ErrProvisioned = errors.New("this dashboard is provisioned from a file")

// DashboardRow is one stored dashboard: the metadata the database owns, plus
// the definition it does not interpret.
//
// It is deliberately not internal/dashboard.Dashboard. This package stores
// rows and must keep working when the definition's shape changes; making it
// import the definition types would put a JSON schema decision in the storage
// layer, and give internal/dashboard and internal/meta a dependency neither
// needs.
type DashboardRow struct {
	ID          int64
	UID         string
	Title       string
	Description string
	Definition  []byte
	Provisioned bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CreateDashboard inserts a definition and returns the row as stored,
// including the id the database assigned.
func (d *DB) CreateDashboard(ctx context.Context, row DashboardRow, now time.Time) (DashboardRow, error) {
	row.CreatedAt = time.Unix(now.Unix(), 0)
	row.UpdatedAt = row.CreatedAt
	res, err := d.db.ExecContext(ctx,
		`INSERT INTO dashboards(uid, title, description, definition, provisioned, created_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		nullIfEmpty(row.UID), row.Title, row.Description, string(row.Definition), row.Provisioned,
		row.CreatedAt.Unix(), row.UpdatedAt.Unix())
	if err != nil {
		return DashboardRow{}, fmt.Errorf("meta: creating dashboard %q: %w", row.Title, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return DashboardRow{}, fmt.Errorf("meta: creating dashboard %q: %w", row.Title, err)
	}
	row.ID = id
	return row, nil
}

// UpdateDashboard replaces a dashboard's definition.
//
// CreatedAt is not touched — it records when the dashboard was first made, and
// an update is not a new dashboard. A provisioned row is refused with
// [ErrProvisioned]; use [DB.UpsertProvisionedDashboard], which is the only
// thing allowed to write one.
func (d *DB) UpdateDashboard(ctx context.Context, id int64, row DashboardRow, now time.Time) (DashboardRow, error) {
	existing, err := d.Dashboard(ctx, id)
	if err != nil {
		return DashboardRow{}, err
	}
	if existing.Provisioned {
		return DashboardRow{}, fmt.Errorf("%w: dashboard %d (%s)", ErrProvisioned, id, existing.Title)
	}
	updated := time.Unix(now.Unix(), 0)
	if _, err := d.db.ExecContext(ctx,
		`UPDATE dashboards SET uid = ?, title = ?, description = ?, definition = ?, updated_at = ?
		 WHERE id = ?`,
		nullIfEmpty(row.UID), row.Title, row.Description, string(row.Definition), updated.Unix(), id); err != nil {
		return DashboardRow{}, fmt.Errorf("meta: updating dashboard %d: %w", id, err)
	}
	row.ID = id
	row.Provisioned = false
	row.CreatedAt = existing.CreatedAt
	row.UpdatedAt = updated
	return row, nil
}

// UpsertProvisionedDashboard writes a dashboard read from a file, keyed by uid.
//
// It is separate from Create and Update because provisioning is the one writer
// allowed to overwrite a provisioned row, and because it must be idempotent:
// it runs at every startup, and a restart that changed nothing must leave
// updated_at alone so that "when did this dashboard last change" keeps meaning
// something. An unchanged definition is therefore not a write at all.
func (d *DB) UpsertProvisionedDashboard(ctx context.Context, row DashboardRow, now time.Time) (DashboardRow, bool, error) {
	if row.UID == "" {
		return DashboardRow{}, false, fmt.Errorf("meta: a provisioned dashboard needs a uid")
	}
	existing, err := d.DashboardByUID(ctx, row.UID)
	switch {
	case errors.Is(err, ErrNoDashboard):
		row.Provisioned = true
		created, err := d.CreateDashboard(ctx, row, now)
		return created, true, err
	case err != nil:
		return DashboardRow{}, false, err
	}
	// A restart with an unedited file is the common case, so it is the one
	// that must not churn the row.
	if existing.Title == row.Title && existing.Description == row.Description &&
		string(existing.Definition) == string(row.Definition) && existing.Provisioned {
		return existing, false, nil
	}
	updated := time.Unix(now.Unix(), 0)
	if _, err := d.db.ExecContext(ctx,
		`UPDATE dashboards SET title = ?, description = ?, definition = ?, provisioned = 1, updated_at = ?
		 WHERE uid = ?`,
		row.Title, row.Description, string(row.Definition), updated.Unix(), row.UID); err != nil {
		return DashboardRow{}, false, fmt.Errorf("meta: provisioning dashboard %q: %w", row.UID, err)
	}
	row.ID = existing.ID
	row.Provisioned = true
	row.CreatedAt = existing.CreatedAt
	row.UpdatedAt = updated
	return row, true, nil
}

// DeleteDashboard removes a dashboard. A provisioned one is refused: deleting
// it would only last until the next startup.
func (d *DB) DeleteDashboard(ctx context.Context, id int64) error {
	existing, err := d.Dashboard(ctx, id)
	if err != nil {
		return err
	}
	if existing.Provisioned {
		return fmt.Errorf("%w: dashboard %d (%s)", ErrProvisioned, id, existing.Title)
	}
	if _, err := d.db.ExecContext(ctx, `DELETE FROM dashboards WHERE id = ?`, id); err != nil {
		return fmt.Errorf("meta: deleting dashboard %d: %w", id, err)
	}
	return nil
}

// Dashboard returns one dashboard by id.
func (d *DB) Dashboard(ctx context.Context, id int64) (DashboardRow, error) {
	return d.scanOne(d.db.QueryRowContext(ctx, dashboardColumns+` WHERE id = ?`, id), fmt.Sprintf("%d", id))
}

// DashboardByUID returns one dashboard by its provisioning uid.
func (d *DB) DashboardByUID(ctx context.Context, uid string) (DashboardRow, error) {
	return d.scanOne(d.db.QueryRowContext(ctx, dashboardColumns+` WHERE uid = ?`, uid), uid)
}

const dashboardColumns = `SELECT id, COALESCE(uid, ''), title, description, definition, provisioned,
	created_at, updated_at FROM dashboards`

func (d *DB) scanOne(row *sql.Row, which string) (DashboardRow, error) {
	var r DashboardRow
	var def string
	var created, updated int64
	err := row.Scan(&r.ID, &r.UID, &r.Title, &r.Description, &def, &r.Provisioned, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return DashboardRow{}, fmt.Errorf("%w: %s", ErrNoDashboard, which)
	}
	if err != nil {
		return DashboardRow{}, fmt.Errorf("meta: reading dashboard %s: %w", which, err)
	}
	r.Definition = []byte(def)
	r.CreatedAt = time.Unix(created, 0)
	r.UpdatedAt = time.Unix(updated, 0)
	return r, nil
}

// Dashboards lists every dashboard, by title.
//
// The definitions come back too. A list of twenty dashboards is a few tens of
// kilobytes, and the alternative — a summary list plus a fetch per row — is
// what makes a dashboard picker feel slow. If a deployment ever has enough
// dashboards for that to be wrong, the fix is a `?fields=` parameter, not a
// second table.
func (d *DB) Dashboards(ctx context.Context) ([]DashboardRow, error) {
	rows, err := d.db.QueryContext(ctx, dashboardColumns+` ORDER BY title, id`)
	if err != nil {
		return nil, fmt.Errorf("meta: listing dashboards: %w", err)
	}
	defer rows.Close()
	var out []DashboardRow
	for rows.Next() {
		var r DashboardRow
		var def string
		var created, updated int64
		if err := rows.Scan(&r.ID, &r.UID, &r.Title, &r.Description, &def, &r.Provisioned, &created, &updated); err != nil {
			return nil, fmt.Errorf("meta: listing dashboards: %w", err)
		}
		r.Definition = []byte(def)
		r.CreatedAt = time.Unix(created, 0)
		r.UpdatedAt = time.Unix(updated, 0)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("meta: listing dashboards: %w", err)
	}
	return out, nil
}

// nullIfEmpty keeps the uid column's UNIQUE constraint useful: SQLite treats
// every NULL as distinct, so many API-created dashboards can have no uid,
// while two provisioned files claiming the same uid still collide — which is
// the collision worth catching.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
