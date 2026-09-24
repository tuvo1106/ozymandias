package block

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// MetaFilename is written last, and its presence is what makes a block real.
// A directory without it is the wreckage of a crashed write and is deleted at
// startup; see [Write].
const MetaFilename = "meta.json"

// MetaVersion is the block layout version. A reader refuses anything else
// rather than guessing, because a misread block is worse than an absent one.
const MetaVersion = 1

// Meta describes a block. It is JSON, not a binary format, because it is small,
// read once per block at startup, and the first thing anyone looks at when
// something is wrong — being able to `cat` it is worth more than its bytes.
type Meta struct {
	ULID    ulid.ULID `json:"ulid"`
	Version int       `json:"version"`
	// MinTime and MaxTime are inclusive, in unix milliseconds.
	MinTime int64 `json:"minTime"`
	MaxTime int64 `json:"maxTime"`
	Stats   Stats `json:"stats"`
	// Compaction records how this block came to be, so compaction can pick
	// its next input and so a human can trace a block back to its sources.
	Compaction Compaction `json:"compaction"`
	// ResolutionS is 0 for raw samples, or the rollup interval in seconds.
	// Rollup blocks (M2 PR 2) sit beside their source at the same time range,
	// so the querier picks by resolution rather than by time.
	ResolutionS int `json:"resolution_s"`
}

// Stats are the block's contents, for `tsdb inspect` and for compaction's
// size heuristics.
type Stats struct {
	Series  int `json:"series"`
	Samples int `json:"samples"`
	Chunks  int `json:"chunks"`
}

// Compaction is a block's provenance.
type Compaction struct {
	// Level is 0 for a block cut from the head, and one more than its sources
	// for a compacted block.
	Level int `json:"level"`
	// Sources are the blocks merged into this one, oldest first. Empty for a
	// level 0 block.
	Sources []ulid.ULID `json:"sources,omitempty"`
}

// entropy is seeded once, at startup, and shared. It must not be derived from
// the timestamp being encoded: that would make the id a pure function of the
// millisecond, so two blocks cut inside the same one would get the same id and
// the second would collide with the first's directory.
//
// ulid.Monotonic gives the rest: within a millisecond it increments the
// previous entropy instead of redrawing, so same-millisecond ids are distinct
// *and* still sort in creation order.
var (
	entropyMu sync.Mutex
	//nolint:gosec // a collision-avoidance id in one directory, not a secret
	entropy = ulid.Monotonic(rand.New(rand.NewSource(time.Now().UnixNano())), 0)
)

// NewULID returns a block id: a lexicographically sortable ulid whose prefix is
// the creation time in milliseconds. Sortable ids mean `ls blocks/` is in
// creation order, and compaction can find adjacent blocks without parsing every
// meta.json.
func NewULID(now time.Time) ulid.ULID {
	entropyMu.Lock()
	defer entropyMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(now), entropy)
}

// ReadMeta loads the meta.json of the block directory dir.
func ReadMeta(dir string) (Meta, error) {
	b, err := os.ReadFile(filepath.Join(dir, MetaFilename))
	if err != nil {
		return Meta{}, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return Meta{}, fmt.Errorf("block: parsing %s: %w", filepath.Join(dir, MetaFilename), err)
	}
	if m.Version != MetaVersion {
		return Meta{}, fmt.Errorf("block: %s has version %d, this build reads %d",
			dir, m.Version, MetaVersion)
	}
	return m, nil
}

func writeMeta(dir string, m Meta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("block: encoding meta: %w", err)
	}
	return writeFileSync(filepath.Join(dir, MetaFilename), append(b, '\n'))
}

// writeFileSync writes a file and fsyncs it. Without the fsync the rename in
// [Write] could land before the contents, leaving a block that exists and is
// empty — the one state the "meta.json means complete" rule cannot detect.
func writeFileSync(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("block: writing %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("block: syncing %s: %w", path, err)
	}
	return f.Close()
}

// syncDir fsyncs a directory so that a rename or create in it is durable. On
// most filesystems the entry is otherwise only in the page cache, and a crash
// can lose a file that was itself safely synced.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil && errors.Is(err, os.ErrInvalid) {
		return nil // some filesystems refuse to sync a directory; not fatal
	}
	return err
}
