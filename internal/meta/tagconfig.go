package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const tagConfigSchema = `
CREATE TABLE IF NOT EXISTS metric_tag_config (
	metric     TEXT PRIMARY KEY,
	mode       TEXT NOT NULL CHECK (mode IN ('allow', 'deny')),
	keys_json  TEXT NOT NULL,           -- a JSON array of tag keys
	updated_at INTEGER NOT NULL         -- unix seconds
) WITHOUT ROWID;
`

// TagConfig is one metric's rule for which tag keys it keeps: in mode "deny" every key
// in Keys is dropped, in mode "allow" only those are kept.
type TagConfig struct {
	Metric    string
	Mode      string
	Keys      []string
	UpdatedAt time.Time
}

// SetTagConfig creates or replaces a metric's tag rule.
func (d *DB) SetTagConfig(ctx context.Context, c TagConfig, now time.Time) error {
	keys, err := json.Marshal(c.Keys)
	if err != nil {
		return err
	}
	d.txMu.Lock()
	defer d.txMu.Unlock()
	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO metric_tag_config (metric, mode, keys_json, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(metric) DO UPDATE SET mode = excluded.mode, keys_json = excluded.keys_json, updated_at = excluded.updated_at`,
		c.Metric, c.Mode, string(keys), now.Unix()); err != nil {
		return fmt.Errorf("meta: saving tag config: %w", err)
	}
	return nil
}

// DeleteTagConfig removes a metric's tag rule. A metric with none is not an error.
func (d *DB) DeleteTagConfig(ctx context.Context, metric string) error {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	if _, err := d.db.ExecContext(ctx, `DELETE FROM metric_tag_config WHERE metric = ?`, metric); err != nil {
		return fmt.Errorf("meta: deleting tag config: %w", err)
	}
	return nil
}

// TagConfigs lists every rule, by metric name.
func (d *DB) TagConfigs(ctx context.Context) ([]TagConfig, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT metric, mode, keys_json, updated_at FROM metric_tag_config ORDER BY metric`)
	if err != nil {
		return nil, fmt.Errorf("meta: listing tag config: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TagConfig
	for rows.Next() {
		var c TagConfig
		var keys string
		var at int64
		if err := rows.Scan(&c.Metric, &c.Mode, &keys, &at); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(keys), &c.Keys); err != nil {
			return nil, fmt.Errorf("meta: tag config for %q: %w", c.Metric, err)
		}
		c.UpdatedAt = time.Unix(at, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}
