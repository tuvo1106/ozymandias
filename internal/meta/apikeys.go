package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// apiKeySchema holds ingest API keys. The key itself is never stored: only its
// SHA-256, so a copy of meta.db (a backup, a leaked volume) cannot be replayed
// against intake. A key is a 256-bit random string, so a plain hash is enough —
// there is nothing to brute-force, which is what a password hash's cost is for.
const apiKeySchema = `
CREATE TABLE IF NOT EXISTS api_keys (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	name         TEXT NOT NULL,
	key_sha256   TEXT NOT NULL UNIQUE,  -- hex
	created_at   INTEGER NOT NULL,      -- unix seconds
	last_used_at INTEGER,               -- NULL: never used
	revoked_at   INTEGER,               -- NULL: active
	limits_json  TEXT NOT NULL DEFAULT '{}'
);
`

// APIKey is one row of api_keys.
type APIKey struct {
	ID         int64
	Name       string
	SHA256     string // hex of the key's SHA-256
	CreatedAt  time.Time
	LastUsedAt time.Time // zero: never
	RevokedAt  time.Time // zero: active
	LimitsJSON string
}

// Active reports whether the key may still be used.
func (k APIKey) Active() bool { return k.RevokedAt.IsZero() }

// ErrNoAPIKey is returned when an id names no key.
var ErrNoAPIKey = errors.New("no such api key")

// CreateAPIKey stores a key by its hash and returns the row.
func (d *DB) CreateAPIKey(ctx context.Context, name, sha256hex, limitsJSON string, now time.Time) (APIKey, error) {
	if limitsJSON == "" {
		limitsJSON = "{}"
	}
	d.txMu.Lock()
	defer d.txMu.Unlock()
	res, err := d.db.ExecContext(ctx,
		`INSERT INTO api_keys (name, key_sha256, created_at, limits_json) VALUES (?, ?, ?, ?)`,
		name, sha256hex, now.Unix(), limitsJSON)
	if err != nil {
		return APIKey{}, fmt.Errorf("meta: creating api key: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return APIKey{}, fmt.Errorf("meta: creating api key: %w", err)
	}
	return APIKey{ID: id, Name: name, SHA256: sha256hex, CreatedAt: time.Unix(now.Unix(), 0), LimitsJSON: limitsJSON}, nil
}

// APIKeys lists every key, active and revoked, oldest first.
func (d *DB) APIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, name, key_sha256, created_at, last_used_at, revoked_at, limits_json FROM api_keys ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("meta: listing api keys: %w", err)
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		var created int64
		var used, revoked sql.NullInt64
		if err := rows.Scan(&k.ID, &k.Name, &k.SHA256, &created, &used, &revoked, &k.LimitsJSON); err != nil {
			return nil, fmt.Errorf("meta: listing api keys: %w", err)
		}
		k.CreatedAt = time.Unix(created, 0)
		if used.Valid {
			k.LastUsedAt = time.Unix(used.Int64, 0)
		}
		if revoked.Valid {
			k.RevokedAt = time.Unix(revoked.Int64, 0)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey marks a key revoked. Revoking a revoked key keeps its first time.
func (d *DB) RevokeAPIKey(ctx context.Context, id int64, now time.Time) error {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	res, err := d.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, now.Unix(), id)
	if err != nil {
		return fmt.Errorf("meta: revoking api key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoAPIKey
	}
	return nil
}

// TouchAPIKey records that a key was used. Callers throttle it: a write per
// request would put a SQLite commit on the intake path.
func (d *DB) TouchAPIKey(ctx context.Context, id int64, now time.Time) error {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	_, err := d.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`, now.Unix(), id)
	if err != nil {
		return fmt.Errorf("meta: touching api key: %w", err)
	}
	return nil
}
