package logstore

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/tuvo1106/ozymandias/internal/query/logql"
)

const catalogSchema = `
CREATE TABLE IF NOT EXISTS log_streams (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	key    TEXT NOT NULL UNIQUE   -- the labels joined, so a stream is found by its identity
);
CREATE TABLE IF NOT EXISTS log_stream_labels (
	stream_id INTEGER NOT NULL REFERENCES log_streams(id),
	k         TEXT NOT NULL,
	v         TEXT NOT NULL,
	PRIMARY KEY (stream_id, k)
) WITHOUT ROWID;
`

// labelSet is a stream's identity: its value for each of logql.StreamLabels,
// in that order. A missing label is "" (the same rule the query side uses).
type labelSet [5]string

func init() {
	// labelSet's size is the number of stream labels; keep them in step.
	if len(logql.StreamLabels) != len(labelSet{}) {
		panic("logstore: labelSet is out of step with logql.StreamLabels")
	}
}

// key is the identity as one string. The separator cannot appear in a label
// (the wire format refuses control characters in service, source and host),
// and status is one of five words; env comes from a tag, which is checked
// separately, so the key is unambiguous.
func (l labelSet) key() string { return strings.Join(l[:], "\x1f") }

// value returns the stream's value for a reserved key, "" for one that is not
// a stream label.
func (l labelSet) value(key string) string {
	for i, k := range logql.StreamLabels {
		if k == key {
			return l[i]
		}
	}
	return ""
}

// catalog is the durable list of streams: which label set has which id. It is
// small (streams are low-cardinality by construction) and everything else in
// the store is rebuilt from files, which is why this is the only thing in
// SQLite.
type catalog struct{ db *sql.DB }

func openCatalog(path string, noSync bool) (*catalog, error) {
	sync := "FULL"
	if noSync {
		sync = "OFF"
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous("+sync+")")
	if err != nil {
		return nil, fmt.Errorf("logstore catalog: %w", err)
	}
	// One connection: writes are rare and serialized by the store anyway, and
	// it removes SQLite's cross-connection locking from the picture.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(catalogSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("logstore catalog %s: %w", path, err)
	}
	return &catalog{db: db}, nil
}

func (c *catalog) close() error { return c.db.Close() }

type streamRow struct {
	id     int64
	labels labelSet
}

// streams loads every stream.
func (c *catalog) streams() ([]streamRow, error) {
	// A join from log_streams, not a scan of the labels: a stream whose labels
	// are all empty has no label rows and must still come back.
	rows, err := c.db.Query(`SELECT s.id, COALESCE(l.k, ''), COALESCE(l.v, '')
		FROM log_streams s LEFT JOIN log_stream_labels l ON l.stream_id = s.id ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[int64]*streamRow{}
	var order []int64
	for rows.Next() {
		var id int64
		var k, v string
		if err := rows.Scan(&id, &k, &v); err != nil {
			return nil, err
		}
		r := byID[id]
		if r == nil {
			r = &streamRow{id: id}
			byID[id] = r
			order = append(order, id)
		}
		for i, name := range logql.StreamLabels {
			if k != "" && name == k {
				r.labels[i] = v
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]streamRow, len(order))
	for i, id := range order {
		out[i] = *byID[id]
	}
	return out, nil
}

// create records a new stream and returns its id. The key row and its label
// rows go in one transaction, so a stream is never half-recorded.
func (c *catalog) create(l labelSet) (int64, error) {
	tx, err := c.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`INSERT INTO log_streams(key) VALUES (?)`, l.key())
	if err != nil {
		return 0, fmt.Errorf("logstore catalog: creating stream: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for i, name := range logql.StreamLabels {
		if l[i] == "" {
			continue // an absent label is stored as nothing
		}
		if _, err := tx.Exec(`INSERT INTO log_stream_labels(stream_id, k, v) VALUES (?, ?, ?)`, id, name, l[i]); err != nil {
			return 0, fmt.Errorf("logstore catalog: creating stream labels: %w", err)
		}
	}
	return id, tx.Commit()
}
