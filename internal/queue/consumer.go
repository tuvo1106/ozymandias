package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// Consumer reads a topic in offset order and commits how far it has got.
//
// It is meant for one goroutine: the store's consumer loop polls a batch,
// applies it, and commits the batch's last offset. Calls are serialized
// anyway, so a stray concurrent call is safe, just pointless.
type Consumer struct {
	t    *Topic
	name string
	path string

	mu sync.Mutex
	// cursor is the next offset Poll returns; (seg, off) is where in the
	// files to look for it. off may point at an earlier record than the
	// cursor — after a restart, reading starts at the top of the cursor's
	// segment and skips forward — but never at a later one.
	cursor uint64
	seg    int
	off    int64
	closed bool
	// done is closed by Close, to wake a Poll that is waiting for records.
	done chan struct{}
}

// Close releases the registration, so the name can be registered again (by
// a supervisor restarting a store's consumer loop, say). The committed
// offset stays behind and keeps holding back retention: a consumer that
// exists on disk is a consumer, whether or not anything is reading for it.
// Polls and Commits on a closed Consumer return ErrClosed.
func (c *Consumer) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.done)
	c.t.mu.Lock()
	delete(c.t.active, c.name)
	c.t.mu.Unlock()
}

// Name is the consumer's name.
func (c *Consumer) Name() string { return c.name }

// Poll returns up to max durable records starting at the cursor, waiting
// until at least one exists or ctx ends. The cursor advances past what is
// returned; nothing is committed.
//
// The consumer's lock is not held while waiting, so a Commit from another
// goroutine (a pipelined apply loop) is never stuck behind an idle Poll.
//
// It returns records or an error, never both: on an error the cursor is put
// back where this Poll started, so the next Poll reads the same records
// again rather than starting after ones the caller was never given.
func (c *Consumer) Poll(ctx context.Context, max int) ([]Record, error) {
	if max <= 0 {
		return nil, fmt.Errorf("queue: Poll max must be positive, got %d", max)
	}
	t := c.t
	for {
		c.mu.Lock()
		cursor, cclosed := c.cursor, c.closed
		c.mu.Unlock()
		if cclosed {
			return nil, ErrClosed
		}
		t.mu.Lock()
		closed, notify, durable, broken := t.closed, t.notify, t.synced, t.broken
		t.mu.Unlock()
		if closed {
			return nil, ErrClosed
		}
		if broken != nil && cursor >= durable {
			// Nothing more will ever become durable; say so rather than
			// wait like an idle consumer on a healthy topic.
			return nil, broken
		}
		if cursor < durable {
			c.mu.Lock()
			if c.cursor < durable { // another Poll may have got there first
				cur, seg, off := c.cursor, c.seg, c.off
				out, err := c.readLocked(max, durable)
				if err != nil {
					c.cursor, c.seg, c.off = cur, seg, off
					out = nil
				}
				c.mu.Unlock()
				return out, err
			}
			c.mu.Unlock()
			continue
		}
		select {
		case <-notify:
		case <-c.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// readLocked reads up to max records below durable from the cursor on. The
// caller holds c.mu and has checked that at least one is there.
func (c *Consumer) readLocked(max int, durable uint64) ([]Record, error) {
	var out []Record
	for len(out) < max && c.cursor < durable {
		got, err := c.readSegment(&out, max, durable)
		if errors.Is(err, os.ErrNotExist) && c.t.retired(c.seg) {
			// Another consumer's commit retired the segment this one was
			// parked at the end of. Everything in it was below every
			// committed offset, this consumer's included, so nothing is
			// skipped by moving on.
			c.seg, c.off = c.t.locate(c.cursor), 0
			continue
		}
		if err != nil {
			return out, err
		}
		if !got && len(out) < max && c.cursor < durable {
			// This segment is exhausted but durable records remain, so
			// they are in the next one.
			next, ok := c.t.segmentAfter(c.seg)
			if !ok {
				return out, fmt.Errorf("%w: offset %d is durable but no segment after %d holds it",
					ErrCorrupt, c.cursor, c.seg)
			}
			c.seg, c.off = next, 0
		}
	}
	return out, nil
}

// readSegment appends records from the current segment to out, stopping at
// max, at the durable end, or at the end of the file. It reports whether it
// stopped for one of the first two reasons (true) or ran out of file (false).
//
// It opens the file afresh on every call rather than holding a reader across
// Polls: a buffered reader may have read ahead into bytes the writer later
// cut back off after a failed write, and must not hand them out.
func (c *Consumer) readSegment(out *[]Record, max int, durable uint64) (bool, error) {
	r := wal.NewSegmentReader(c.t.dir, c.seg, c.off)
	defer func() { _ = r.Close() }()
	for len(*out) < max && c.cursor < durable {
		if !r.Next() {
			if err := r.Err(); err != nil {
				return false, fmt.Errorf("queue: consumer %q: %w", c.name, err)
			}
			return false, nil
		}
		off, err := recordOffset(r.Record())
		if err != nil {
			return false, fmt.Errorf("consumer %q, segment %d: %w", c.name, c.seg, err)
		}
		c.off = r.End().Offset
		switch {
		case off < c.cursor:
			continue // positioning after a restart: skipped without copying the payload
		case off > c.cursor:
			return false, fmt.Errorf("%w: segment %d jumps from offset %d to %d",
				ErrCorrupt, c.seg, c.cursor, off)
		}
		rec, err := decodeRecord(r.Record())
		if err != nil {
			return false, fmt.Errorf("consumer %q, segment %d: %w", c.name, c.seg, err)
		}
		*out = append(*out, rec)
		c.cursor++
	}
	return true, nil
}

// Commit records that every offset up to and including offset has been
// processed, durably: after it returns, a restart resumes at offset+1. It
// then runs a retention pass; a retention failure is not a commit failure
// and is reported in [Stats].RetentionErr instead.
// Committing an offset at or below an earlier commit is a no-op, so a
// consumer may re-commit after a retry without checking.
//
// Committing past the cursor is an error — it would skip records nobody
// read — as is committing on a closed topic.
func (c *Consumer) Commit(offset uint64) error {
	advanced, err := c.commit(offset)
	if err != nil || !advanced {
		return err
	}
	// Outside c.mu: a retention pass unlinks files and lists the
	// directory, and this consumer's Poll should not wait on that.
	// Failures surface in Stats.RetentionErr: the commit itself is durable.
	c.t.retain()
	return nil
}

// commit writes the offset file under c.mu, which serializes commits so a
// slower, older one can never overwrite a newer one. It reports whether the
// committed offset moved.
func (c *Consumer) commit(offset uint64) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false, ErrClosed
	}
	if offset >= c.cursor {
		return false, fmt.Errorf("queue: consumer %q cannot commit offset %d: it has only read up to %d",
			c.name, offset, int64(c.cursor)-1)
	}
	t := c.t
	t.mu.Lock()
	closed, prev := t.closed, t.committed[c.name]
	t.mu.Unlock()
	if closed {
		return false, ErrClosed
	}
	next := offset + 1
	if next <= prev {
		return false, nil
	}
	if err := writeOffsetFile(c.path, next); err != nil {
		return false, err
	}
	t.mu.Lock()
	t.committed[c.name] = next
	t.mu.Unlock()
	return true, nil
}

// retired reports whether retention has deleted segment seg.
//
// It takes retMu first, because a retention pass unlinks the files before it
// updates segs: without the lock, a consumer that found its file gone could
// look in that gap, see the segment still listed, and report a missing file
// as corruption. With it, the pass is either finished or not started.
func (t *Topic) retired(seg int) bool {
	t.retMu.Lock()
	defer t.retMu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	return seg < t.segs[0].n
}

// locate returns the segment that holds offset off: the newest one starting
// at or before it.
func (t *Topic) locate(off uint64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	seg := t.segs[0].n
	for _, s := range t.segs {
		if s.first <= off {
			seg = s.n
		}
	}
	return seg
}

// segmentAfter returns the number of the segment following seg.
func (t *Topic) segmentAfter(seg int) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.segs {
		if s.n > seg {
			return s.n, true
		}
	}
	return 0, false
}
