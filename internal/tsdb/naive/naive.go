package naive

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sync"

	_ "modernc.org/sqlite" // registers the "sqlite" driver: pure Go, no cgo

	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

const schema = `
CREATE TABLE IF NOT EXISTS series (
	id        INTEGER PRIMARY KEY,
	metric    TEXT NOT NULL,
	key       TEXT NOT NULL UNIQUE,  -- SeriesRef.Key(): the identity
	tags_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS series_metric ON series(metric, key);

CREATE TABLE IF NOT EXISTS series_tags (
	series_id INTEGER NOT NULL REFERENCES series(id),
	key       TEXT NOT NULL,
	value     TEXT NOT NULL,
	PRIMARY KEY (series_id, key, value)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS series_tags_kv ON series_tags(key, value);

CREATE TABLE IF NOT EXISTS samples (
	series_id INTEGER NOT NULL,
	t         INTEGER NOT NULL,  -- unix milliseconds
	v         REAL NOT NULL,
	PRIMARY KEY (series_id, t)
) WITHOUT ROWID;
`

// Store is the naive MetricStore. It trades every kind of efficiency for
// being obviously correct: one row per sample, selection by scanning a
// metric's series and applying tsdb.Matcher's reference semantics in Go.
// That is exactly what makes it a good oracle for M2's differential tests.
type Store struct {
	db *sql.DB

	mu  sync.Mutex // serializes writers; SQLite allows one at a time anyway
	ids map[string]int64
}

var _ tsdb.MetricStore = (*Store)(nil)

// Open opens (creating if needed) the store at path.
func Open(path string) (*Store, error) {
	// WAL lets readers (queries) proceed while a writer (intake) appends;
	// synchronous=NORMAL is durable across process crashes, and across power
	// loss up to the last checkpoint — fine for a store that's a stopgap.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("naive store: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("naive store %s: %w", path, err)
	}
	type idKey struct {
		id  int64
		key string
	}
	all, err := query(context.Background(), db, func(r *sql.Rows) (ik idKey, err error) {
		return ik, r.Scan(&ik.id, &ik.key)
	}, `SELECT id, key FROM series`)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("naive store: loading series: %w", err)
	}
	s := &Store{db: db, ids: make(map[string]int64, len(all))}
	for _, ik := range all {
		s.ids[ik.key] = ik.id
	}
	return s, nil
}

// query runs q and scans every row with scan. One place for the
// Query/Next/Scan/Err/Close dance, so each caller is just its SQL.
func query[T any](ctx context.Context, db *sql.DB, scan func(*sql.Rows) (T, error), q string, args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Append stores a batch in one transaction. A sample at an existing
// (series, t) replaces the old value — last write wins.
func (s *Store) Append(ctx context.Context, batch []tsdb.SeriesSamples) (tsdb.AppendResult, error) {
	var res tsdb.AppendResult
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()
	newIDs := map[string]int64{}

	insSample, err := tx.PrepareContext(ctx, `INSERT INTO samples(series_id, t, v) VALUES(?, ?, ?)
		ON CONFLICT(series_id, t) DO UPDATE SET v = excluded.v`)
	if err != nil {
		return res, err
	}
	defer insSample.Close()

	for _, ss := range batch {
		if reason := check(ss); reason != "" {
			res.Rejected = append(res.Rejected, tsdb.Rejected{Series: ss.Series, Reason: reason})
			continue
		}
		key := ss.Series.Key()
		id, ok := s.ids[key]
		if !ok {
			if id, ok = newIDs[key]; !ok {
				if id, err = insertSeries(ctx, tx, ss.Series, key); err != nil {
					return tsdb.AppendResult{}, err
				}
				newIDs[key] = id
			}
		}
		for _, smp := range ss.Samples {
			if _, err := insSample.ExecContext(ctx, id, smp.T, smp.V); err != nil {
				return tsdb.AppendResult{}, err
			}
		}
		res.Series++
		res.Samples += len(ss.Samples)
	}
	if err := tx.Commit(); err != nil {
		return tsdb.AppendResult{}, err
	}
	for k, id := range newIDs { // only after commit: a rollback must not leave stale ids
		s.ids[k] = id
	}
	return res, nil
}

func check(ss tsdb.SeriesSamples) string {
	if err := ss.Series.Validate(); err != nil {
		return err.Error()
	}
	for _, smp := range ss.Samples {
		if math.IsNaN(smp.V) || math.IsInf(smp.V, 0) {
			return "non-finite sample value"
		}
	}
	return ""
}

func insertSeries(ctx context.Context, tx *sql.Tx, ref tsdb.SeriesRef, key string) (int64, error) {
	tags := make([]string, len(ref.Tags))
	for i, t := range ref.Tags {
		tags[i] = t.String()
	}
	tagsJSON, _ := json.Marshal(tags) // a []string always marshals
	r, err := tx.ExecContext(ctx, `INSERT INTO series(metric, key, tags_json) VALUES(?, ?, ?)`, ref.Metric, key, string(tagsJSON))
	if err != nil {
		return 0, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, t := range ref.Tags {
		if _, err := tx.ExecContext(ctx, `INSERT INTO series_tags(series_id, key, value) VALUES(?, ?, ?)`, id, t.Key, t.Value); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// Select returns the selected series with samples in [fromMs, toMs].
func (s *Store) Select(ctx context.Context, sel tsdb.Selector, fromMs, toMs int64) (tsdb.SeriesSet, error) {
	type cand struct {
		id  int64
		ref tsdb.SeriesRef
	}
	all, err := query(ctx, s.db, func(r *sql.Rows) (c cand, err error) {
		var tagsJSON string
		if err := r.Scan(&c.id, &tagsJSON); err != nil {
			return c, err
		}
		var tags []string
		if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
			return c, fmt.Errorf("series %d: corrupt tags: %w", c.id, err)
		}
		c.ref = tsdb.NewSeriesRef(sel.Metric, tags)
		return c, nil
	}, `SELECT id, tags_json FROM series WHERE metric = ? ORDER BY key`, sel.Metric)
	if err != nil {
		return nil, err
	}

	var out []tsdb.SeriesSamples
	for _, c := range all {
		if !sel.Matches(c.ref) {
			continue
		}
		samples, err := s.samples(ctx, c.id, fromMs, toMs)
		if err != nil {
			return nil, err
		}
		if len(samples) > 0 {
			out = append(out, tsdb.SeriesSamples{Series: c.ref, Samples: samples})
		}
	}
	return tsdb.NewSliceSet(out), nil
}

func (s *Store) samples(ctx context.Context, id, fromMs, toMs int64) ([]tsdb.Sample, error) {
	return query(ctx, s.db, func(r *sql.Rows) (smp tsdb.Sample, err error) {
		return smp, r.Scan(&smp.T, &smp.V)
	}, `SELECT t, v FROM samples WHERE series_id = ? AND t BETWEEN ? AND ? ORDER BY t`, id, fromMs, toMs)
}

// MetricNames returns metric names starting with prefix, sorted, at most
// limit of them (limit <= 0: all). The prefix test is substr() rather than
// LIKE, which would need '%' and '_' in user input escaped.
func (s *Store) MetricNames(ctx context.Context, prefix string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	return s.strings(ctx, `SELECT DISTINCT metric FROM series WHERE substr(metric, 1, ?) = ? ORDER BY metric LIMIT ?`,
		len(prefix), prefix, limit)
}

// TagKeys returns the tag keys used by any series of metric, sorted.
func (s *Store) TagKeys(ctx context.Context, metric string) ([]string, error) {
	return s.strings(ctx, `SELECT DISTINCT t.key FROM series_tags t JOIN series s ON s.id = t.series_id
		WHERE s.metric = ? ORDER BY t.key`, metric)
}

// TagValues returns the non-empty values of key across metric's series,
// sorted, at most limit of them (limit <= 0: all).
func (s *Store) TagValues(ctx context.Context, metric, key string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = -1
	}
	return s.strings(ctx, `SELECT DISTINCT t.value FROM series_tags t JOIN series s ON s.id = t.series_id
		WHERE s.metric = ? AND t.key = ? AND t.value <> '' ORDER BY t.value LIMIT ?`, metric, key, limit)
}

func (s *Store) strings(ctx context.Context, q string, args ...any) ([]string, error) {
	return query(ctx, s.db, func(r *sql.Rows) (v string, err error) { return v, r.Scan(&v) }, q, args...)
}

// Stats counts series and samples. It runs COUNT(*) queries — fine for a
// store this size, and the reason it isn't the store the real system uses.
func (s *Store) Stats() tsdb.StoreStats {
	var st tsdb.StoreStats
	_ = s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM series), (SELECT COUNT(*) FROM samples)`).Scan(&st.Series, &st.Samples)
	return st
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }
