package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/meta"
)

// keyPrefix makes a key recognizable in a config file or a leaked log, and lets
// a secret scanner be pointed at it.
const keyPrefix = "ozy_"

// GenerateKey returns a new random key and the hex SHA-256 that is stored.
func GenerateKey() (key, sha256hex string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("auth: reading randomness: %w", err)
	}
	key = keyPrefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
	return key, hashKey(key), nil
}

func hashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// KeyStore is the part of internal/meta the keyring needs.
type KeyStore interface {
	CreateAPIKey(ctx context.Context, name, sha256hex, limitsJSON string, now time.Time) (meta.APIKey, error)
	APIKeys(ctx context.Context) ([]meta.APIKey, error)
	RevokeAPIKey(ctx context.Context, id int64, now time.Time) error
	TouchAPIKey(ctx context.Context, id int64, now time.Time) error
}

// touchEvery bounds how often a key's last_used_at is written.
const touchEvery = time.Minute

type entry struct {
	principal *Principal
	revoked   bool
	touched   time.Time
}

// Keyring authenticates presented keys. It holds every key in memory, so a
// request costs a hash and a few comparisons and no SQLite read; Create and
// Revoke update it as well as the database.
type Keyring struct {
	store    KeyStore
	clk      clock.Clock
	defaults Limits

	mu     sync.RWMutex
	byHash map[[sha256.Size]byte]*entry
	rej    *Rejections
}

// SetRejections gives every principal, present and future, the counters its
// span and log limits report to. Called once, before serving.
func (k *Keyring) SetRejections(r *Rejections) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rej = r
	for _, e := range k.byHash {
		e.principal.rej = r
	}
}

// NewKeyring loads the stored keys.
func NewKeyring(ctx context.Context, store KeyStore, clk clock.Clock, defaults Limits) (*Keyring, error) {
	k := &Keyring{store: store, clk: clk, defaults: defaults, byHash: map[[sha256.Size]byte]*entry{}}
	rows, err := store.APIKeys(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		k.add(r)
	}
	return k, nil
}

func (k *Keyring) add(r meta.APIKey) {
	var l Limits
	_ = json.Unmarshal([]byte(r.LimitsJSON), &l) // a bad limits blob means the defaults; Create validates what it writes
	raw, err := hex.DecodeString(r.SHA256)
	if err != nil || len(raw) != sha256.Size {
		return // not a key we could ever match
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	p := newPrincipal(r.ID, r.Name, l.withDefaults(k.defaults), k.clk)
	p.rej = k.rej
	var h [sha256.Size]byte
	copy(h[:], raw)
	k.byHash[h] = &entry{revoked: !r.Active(), touched: r.LastUsedAt, principal: p}
}

// Authenticate returns the principal for a presented key.
//
// It looks the key's SHA-256 up in a map. A lookup that returns early on a
// near match would leak timing if it were keyed by the secret; keyed by the hash
// of a random 256-bit secret it leaks nothing an attacker can use (a prefix of
// the hash says nothing about the key), and it costs one hash however many keys,
// revoked ones included, have ever been made.
func (k *Keyring) Authenticate(presented string) (*Principal, bool) {
	if !strings.HasPrefix(presented, keyPrefix) {
		return nil, false
	}
	sum := sha256.Sum256([]byte(presented))
	k.mu.RLock()
	hit := k.byHash[sum]
	revoked := hit != nil && hit.revoked
	k.mu.RUnlock()
	if hit == nil || revoked {
		return nil, false
	}
	k.touch(hit)
	return hit.principal, true
}

func (k *Keyring) touch(e *entry) {
	now := k.clk.Now()
	k.mu.Lock()
	due := now.Sub(e.touched) >= touchEvery
	if due {
		e.touched = now
	}
	k.mu.Unlock()
	if due {
		// Off the request path: the write takes the metadata database's writer
		// lock, which a dashboard save or a monitor evaluation may hold, and an
		// ingest request must not wait on that. Once a minute per key, so there is
		// no pile-up; best effort, since last_used_at is for people.
		go func() { _ = k.store.TouchAPIKey(context.Background(), e.principal.ID, now) }()
	}
}

// Create makes a key. The returned secret is the only time it exists in the clear.
func (k *Keyring) Create(ctx context.Context, name string, limits Limits) (secret string, row meta.APIKey, err error) {
	if strings.TrimSpace(name) == "" {
		return "", meta.APIKey{}, fmt.Errorf("auth: a key needs a name")
	}
	secret, hash, err := GenerateKey()
	if err != nil {
		return "", meta.APIKey{}, err
	}
	blob, _ := json.Marshal(limits)
	row, err = k.store.CreateAPIKey(ctx, name, hash, string(blob), k.clk.Now())
	if err != nil {
		return "", meta.APIKey{}, err
	}
	k.add(row)
	return secret, row, nil
}

// Revoke stops a key working, at once.
func (k *Keyring) Revoke(ctx context.Context, id int64) error {
	if err := k.store.RevokeAPIKey(ctx, id, k.clk.Now()); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, e := range k.byHash {
		if e.principal.ID == id {
			e.revoked = true
		}
	}
	return nil
}

// List returns the stored keys, for the admin API. Hashes are not for display.
func (k *Keyring) List(ctx context.Context) ([]meta.APIKey, error) { return k.store.APIKeys(ctx) }
