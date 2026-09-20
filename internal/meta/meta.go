package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

const schema = `
CREATE TABLE IF NOT EXISTS metric_meta (
	metric     TEXT PRIMARY KEY,
	type       TEXT NOT NULL,     -- count | rate | gauge
	interval   INTEGER NOT NULL,  -- seconds; 0 for gauge
	first_seen INTEGER NOT NULL   -- unix seconds
) WITHOUT ROWID;
`

// Metric is what ozyd knows about a metric name. A metric's type is
// fixed the first time it is seen: the query layer needs it to aggregate over
// time correctly (a count sums, a gauge averages), and a name whose type
// flips between reports would make every past query ambiguous.
type Metric struct {
	Name      string
	Type      wire.Kind
	Interval  int64
	FirstSeen time.Time
}

// ErrTypeConflict is returned by Observe when a metric arrives with a
// different type than it was first seen with.
var ErrTypeConflict = errors.New("metric type conflict")

// DB is the metadata database. Reads are served from memory — every intake
// request checks every metric — and writes go through to SQLite.
type DB struct {
	db *sql.DB

	mu      sync.RWMutex
	metrics map[string]Metric
}

// Open opens (creating if needed) the metadata database at path.
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("meta: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("meta %s: %w", path, err)
	}
	d := &DB{db: db, metrics: map[string]Metric{}}
	if err := d.load(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("meta: loading metrics: %w", err)
	}
	return d, nil
}

func (d *DB) load() error {
	rows, err := d.db.Query(`SELECT metric, type, interval, first_seen FROM metric_meta`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var m Metric
		var first int64
		if err := rows.Scan(&m.Name, &m.Type, &m.Interval, &first); err != nil {
			return err
		}
		m.FirstSeen = time.Unix(first, 0)
		d.metrics[m.Name] = m
	}
	return rows.Err()
}

// Observe records that metric arrived with this type and interval. The first
// sighting is stored; a later one with the same type is a no-op, and one
// with a different type returns ErrTypeConflict (the series is rejected).
// A changed interval for the same type is accepted and ignored: the flush
// interval is agent configuration, and changing it isn't a new metric.
func (d *DB) Observe(ctx context.Context, metric string, kind wire.Kind, interval int64, now time.Time) error {
	d.mu.RLock()
	m, ok := d.metrics[metric]
	d.mu.RUnlock()
	if ok {
		return conflict(m, kind)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if m, ok := d.metrics[metric]; ok { // lost a race with another writer
		return conflict(m, kind)
	}
	m = Metric{Name: metric, Type: kind, Interval: interval, FirstSeen: time.Unix(now.Unix(), 0)}
	if _, err := d.db.ExecContext(ctx, `INSERT INTO metric_meta(metric, type, interval, first_seen) VALUES(?, ?, ?, ?)`,
		m.Name, string(m.Type), m.Interval, m.FirstSeen.Unix()); err != nil {
		return fmt.Errorf("meta: recording %s: %w", metric, err)
	}
	d.metrics[metric] = m
	return nil
}

func conflict(m Metric, kind wire.Kind) error {
	if m.Type != kind {
		return fmt.Errorf("%w: %s is a %s, not a %s", ErrTypeConflict, m.Name, m.Type, kind)
	}
	return nil
}

// Metric returns what is known about name.
func (d *DB) Metric(name string) (Metric, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	m, ok := d.metrics[name]
	return m, ok
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }
