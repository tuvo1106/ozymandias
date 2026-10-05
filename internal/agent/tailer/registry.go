package tailer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is what the agent remembers about one thing it tails.
type Entry struct {
	// Path is informational (the file's last known path, or a container name).
	Path string `json:"path"`
	// Offset is the byte offset in a file up to which every line has been
	// acknowledged by the intake. It is never ahead of an acknowledgement.
	Offset int64 `json:"offset"`
	// TS is a container's last acknowledged log timestamp, in unix nanoseconds.
	TS int64 `json:"ts,omitempty"`
	// LastSeen is unix seconds, for pruning entries of things that are gone.
	LastSeen int64 `json:"last_seen"`
}

// Registry persists [Entry] values by key so a restart resumes where the last
// acknowledged line ended, instead of re-sending a file or skipping what was
// written while the agent was down.
//
// It is a JSON file replaced atomically (write a temp file, fsync, rename): a
// crash leaves the old registry or the new one, never half of one. Losing the
// registry is safe, only wasteful or lossy at the edges depending on
// start_position: the agent starts those files over from its configured
// start position. A registry that cannot be parsed is treated as empty for the
// same reason, and the corruption is reported to the caller.
type Registry struct {
	path string
	// noSync skips the fsync, for tests that simulate crashes at the logical level and would
	// otherwise spend their time on the disk.
	noSync bool
	mu     sync.Mutex
	m      map[string]Entry
	dirt   bool
}

// OpenRegistry loads the registry at path ("" keeps it in memory only). A
// missing file is an empty registry; an unreadable or corrupt one is too, with
// the error returned alongside a usable registry so the caller can log it.
func OpenRegistry(path string) (*Registry, error) {
	r := &Registry{path: path, m: map[string]Entry{}}
	if path == "" {
		return r, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("tailer: reading registry: %w", err)
	}
	if err := json.Unmarshal(data, &r.m); err != nil {
		r.m = map[string]Entry{}
		return r, fmt.Errorf("tailer: registry %s is corrupt, starting empty: %w", path, err)
	}
	return r, nil
}

// Get returns the entry for key.
func (r *Registry) Get(key string) (Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[key]
	return e, ok
}

// Set records e under key.
func (r *Registry) Set(key string, e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.m[key]; ok {
		if old == e {
			return
		}
		// Only the liveness stamp moved: every poll of every tracked file says "seen
		// now", and writing (and fsyncing) the whole registry each second for that
		// is steady disk wear that grows with the number of files. The stamp only
		// has to be fresher than the prune cutoff, which is days.
		if e.LastSeen-old.LastSeen < lastSeenRefresh && old.Path == e.Path && old.Offset == e.Offset && old.TS == e.TS {
			return
		}
	}
	r.m[key] = e
	r.dirt = true
}

// RegistryTTL is how long an entry for a file or container nobody has seen is
// kept before a scan forgets it: long enough for a service that is down over a
// holiday to resume where it stopped, short enough that churned containers
// (one entry each, for ever) do not grow the file without bound.
const RegistryTTL = 30 * 24 * time.Hour

// lastSeenRefresh is how stale a stored LastSeen may get, in seconds, before a
// poll that read nothing rewrites it.
const lastSeenRefresh = 600

// Delete forgets key.
func (r *Registry) Delete(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[key]; ok {
		delete(r.m, key)
		r.dirt = true
	}
}

// Prune forgets entries not seen since before cutoff and returns how many.
func (r *Registry) Prune(cutoff time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for k, e := range r.m {
		if e.LastSeen < cutoff.Unix() {
			delete(r.m, k)
			n++
		}
	}
	if n > 0 {
		r.dirt = true
	}
	return n
}

// Len is the number of entries.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.m)
}

// Flush writes the registry if it changed since the last write.
func (r *Registry) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.path == "" || !r.dirt {
		return nil
	}
	data, err := json.MarshalIndent(r.m, "", " ")
	if err != nil {
		return fmt.Errorf("tailer: encoding registry: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return fmt.Errorf("tailer: registry dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".registry-*")
	if err != nil {
		return fmt.Errorf("tailer: registry temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // a no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("tailer: writing registry: %w", err)
	}
	if r.noSync {
		// no fsync
	} else if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("tailer: syncing registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("tailer: closing registry: %w", err)
	}
	if err := os.Rename(tmp.Name(), r.path); err != nil {
		return fmt.Errorf("tailer: replacing registry: %w", err)
	}
	r.dirt = false
	return nil
}
