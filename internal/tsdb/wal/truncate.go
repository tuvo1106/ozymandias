package wal

import (
	"fmt"
	"os"
	"path/filepath"
)

// TruncateTail cuts the log back to pos, discarding a torn tail found by a
// Reader. It must be called before any new append, and only on a log that is
// not open for writing.
//
// Keeping the tail instead is not an option: the next append would sit behind
// a partial record, so replay would stop before ever reaching it.
func TruncateTail(dir string, pos Position) error {
	if pos.File == "" {
		return nil // nothing was read; nothing to cut
	}
	path := filepath.Join(dir, pos.File)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("wal: stat %s: %w", pos.File, err)
	}
	// Truncate only if there is something to cut, but run the sweep below
	// either way: whether this one file happens to end where the last good
	// record did says nothing about the segments after it, and returning early
	// on it would leave them in place.
	if info.Size() != pos.Offset {
		if err := os.Truncate(path, pos.Offset); err != nil {
			return fmt.Errorf("wal: truncating %s to %d: %w", pos.File, pos.Offset, err)
		}
	}
	// Any segment *after* the damaged one holds records that replay can never
	// reach, so leaving them would strand data and confuse the next replay.
	segs, err := listNumbered(dir, "", segmentExt)
	if err != nil {
		return err
	}
	for _, n := range segs {
		if pos.Segment >= 0 && n > pos.Segment {
			if err := os.Remove(filepath.Join(dir, fmt.Sprintf("%08d%s", n, segmentExt))); err != nil {
				return fmt.Errorf("wal: removing stranded segment %d: %w", n, err)
			}
		}
	}
	return syncDir(dir)
}

// Repair readies a log for appending after a crash, discarding a torn record
// at the end if there is one. It reports whether it cut anything.
//
// It has to run before the log is opened for writing, and running it is not
// optional. A crash can leave a partial record at the end of the last segment;
// replay stops there, correctly and quietly, because a torn tail is the
// expected shape of an interrupted write. But an appender that then writes
// *behind* that record has put its data somewhere no replay will ever reach —
// so the process acknowledges writes, serves them from memory, and loses every
// one of them at the next restart. Reading the log without repairing it turns
// one lost record into unbounded silent loss.
//
// Damage that is not a torn tail — a bad checksum in the middle of the log —
// is returned as an error and nothing is cut. That is a corrupt log, not an
// interrupted write, and it is not this function's business to decide how much
// of it to throw away.
func Repair(dir string) (bool, error) {
	r, err := NewReader(dir)
	if err != nil {
		return false, err
	}
	// Drain: the position of the last intact record is what this is after,
	// not the records themselves.
	for r.Next() {
	}
	readErr, pos := r.Err(), r.End()
	if err := r.Close(); err != nil {
		return false, err
	}
	if readErr != nil {
		return false, readErr
	}
	if pos.File == "" {
		return false, nil // an empty or absent log has no tail to cut
	}
	info, err := os.Stat(filepath.Join(dir, pos.File))
	if err != nil {
		return false, fmt.Errorf("wal: stat %s: %w", pos.File, err)
	}
	// A segment that is unreadable from its very first byte counts too: the
	// reader opens it, reads nothing, and leaves the position at its offset 0,
	// so the size check sees the difference. Segments beyond that one are
	// [TruncateTail]'s business.
	if info.Size() == pos.Offset {
		return false, nil
	}
	return true, TruncateTail(dir, pos)
}

// Truncate deletes every segment before `before`, after copying forward the
// records that later segments still depend on.
//
// keep decides what survives: the TSDB keeps series records, because a sample
// record names its series by an id defined in one. Returning false for a
// record means "this is already durable elsewhere" — for samples, that they
// are in a block on disk.
//
// The checkpoint is written to a temp file and renamed, so a crash leaves
// either the old segments or a complete checkpoint, never a half-written one
// that replay would trust.
func Truncate(dir string, before int, keep func(Record) bool) error {
	segs, err := listNumbered(dir, "", segmentExt)
	if err != nil {
		return err
	}
	var doomed []int
	for _, n := range segs {
		if n < before {
			doomed = append(doomed, n)
		}
	}
	if len(doomed) == 0 {
		return nil
	}

	if keep != nil {
		if err := writeCheckpoint(dir, doomed, before, keep); err != nil {
			return err
		}
	}
	for _, n := range doomed {
		if err := os.Remove(filepath.Join(dir, fmt.Sprintf("%08d%s", n, segmentExt))); err != nil {
			return fmt.Errorf("wal: removing segment %d: %w", n, err)
		}
	}
	// Old checkpoints are superseded by the one just written.
	cps, err := listNumbered(dir, checkpointPre, "")
	if err != nil {
		return err
	}
	for _, n := range cps {
		if n < before {
			if err := os.Remove(filepath.Join(dir, fmt.Sprintf("%s%08d", checkpointPre, n))); err != nil {
				return fmt.Errorf("wal: removing checkpoint %d: %w", n, err)
			}
		}
	}
	return syncDir(dir)
}

func writeCheckpoint(dir string, doomed []int, before int, keep func(Record) bool) error {
	final := filepath.Join(dir, fmt.Sprintf("%s%08d", checkpointPre, before))
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("wal: creating checkpoint: %w", err)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp) // no-op once the rename has happened
	}()

	w := &WAL{dir: dir, segmentSize: 1 << 62, f: f} // one file, never rolls
	// Existing checkpoints come first: a series record copied forward once may
	// need copying forward again.
	cps, err := listNumbered(dir, checkpointPre, "")
	if err != nil {
		return err
	}
	var sources []string
	for _, n := range cps {
		// n <= before, not n < before. A checkpoint at exactly `before`
		// exists when an earlier Truncate(before) published one and then died
		// partway through deleting its segments — the caller only logs that
		// failure, so the next maintenance pass recomputes the same `before`
		// and runs again. That second run sees only the segments that
		// survived, and the rename at the end lands on checkpoint.before. If
		// it is not read as a source first, every record that lived only in
		// the already-deleted segments goes with it: acknowledged samples in
		// no block and no log.
		if n <= before {
			sources = append(sources, fmt.Sprintf("%s%08d", checkpointPre, n))
		}
	}
	for _, n := range doomed {
		sources = append(sources, fmt.Sprintf("%08d%s", n, segmentExt))
	}
	for _, name := range sources {
		if err := copyKept(dir, name, w, keep); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("wal: syncing checkpoint: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("wal: closing checkpoint: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("wal: publishing checkpoint: %w", err)
	}
	return syncDir(dir)
}

// copyKept replays one file and appends the records keep accepts.
func copyKept(dir, name string, w *WAL, keep func(Record) bool) error {
	r := &Reader{dir: dir, files: []string{name}, end: Position{Segment: -1}, strict: true}
	defer func() { _ = r.Close() }()
	for r.Next() {
		rec := r.Record()
		if !keep(rec) {
			continue
		}
		// Copy: Record.Data aliases the reader's buffer, which is reused.
		data := make([]byte, len(rec.Data))
		copy(data, rec.Data)
		if err := w.Log(Record{Type: rec.Type, Data: data}); err != nil {
			return err
		}
	}
	if err := r.Err(); err != nil {
		return fmt.Errorf("wal: checkpointing %s: %w", name, err)
	}
	return nil
}
