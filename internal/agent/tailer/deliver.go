package tailer

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/logpipeline"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Sink receives batches of logs and returns nil only once they are durably
// accepted downstream (the intake answered 2xx). The tailer commits a file's
// offset only after Send returns nil: that ordering is the whole of its
// at-least-once guarantee.
type Sink interface {
	Send(ctx context.Context, logs []wire.Log) error
}

// DefaultBatchLogs is how many logs one Send carries at most; it is the wire's
// per-request limit.
const DefaultBatchLogs = wire.MaxLogsPerRequest

// stream is what one tailed thing (a file, a container) feeds: the
// multiline joiner, the pipeline, and the sink, in that order.
type stream struct {
	ml    *multiline
	pl    *logpipeline.Pipeline
	meta  logpipeline.Meta
	sink  Sink
	batch int
}

func newStream(pl *logpipeline.Pipeline, meta logpipeline.Meta, start *regexp.Regexp, sink Sink, batch int) *stream {
	if batch <= 0 || batch > wire.MaxLogsPerRequest {
		batch = DefaultBatchLogs
	}
	return &stream{ml: newMultiline(start), pl: pl, meta: meta, sink: sink, batch: batch}
}

// maxBatchRawBytes bounds a batch by the bytes of the lines behind it, not only
// by their count: ozyd refuses a body over wire.MaxDecompressedBytes (or 4 MiB
// gzip'd) with a 413, which the sink treats as poison and drops, committing the
// offset. 1000 lines of 20 KiB would be that, and the whole batch would be lost.
// JSON framing and parsed attributes can multiply a line's size several times;
// an eighth of the limit leaves room for that.
const maxBatchRawBytes = wire.MaxDecompressedBytes / 8

// deliver pushes events through the pipeline and the sink in batches. It
// returns the end of the last event whose logs the sink accepted, which is the
// offset that may be committed; on error that is how far it got, and the
// caller rewinds to it.
//
// Events the pipeline turns into no logs (excluded, rate-limited, or part of a
// Rails request still open) count as delivered: there is nothing to send for
// them, and re-reading them after a crash would only exclude them again. The
// exception is a request still open in the pipeline, which a crash loses; that
// is a documented limit (docs/operations.md).
//
// commit, if set, is called with the new committed position right after each
// batch the sink accepted, not once at the end. That is what bounds the repeat
// after a crash to one batch: the position is made durable (the caller flushes
// the registry) between batches, so a kill can only land between a batch's
// acknowledgement and its commit.
func (s *stream) deliver(ctx context.Context, events []event, committed int64, now time.Time, commit func(end int64)) (int64, error) {
	var logs []wire.Log
	rawBytes := 0
	lastEnd := committed
	flush := func(end int64) error {
		if len(logs) > 0 {
			if err := s.sink.Send(ctx, logs); err != nil {
				return fmt.Errorf("sending %d logs: %w", len(logs), err)
			}
			logs = logs[:0]
		}
		rawBytes = 0
		lastEnd = end
		if commit != nil {
			commit(end)
		}
		return nil
	}
	for _, e := range events {
		m := s.meta
		m.Received = now
		m.Stderr = e.stderr
		out := s.pl.Process(e.text, m)
		logs = append(logs, out...)
		if len(out) > 0 {
			rawBytes += len(e.text)
		}
		if len(logs) >= s.batch || rawBytes >= maxBatchRawBytes {
			if err := flush(e.end); err != nil {
				return lastEnd, err
			}
		}
	}
	if len(events) > 0 {
		if err := flush(events[len(events)-1].end); err != nil {
			return lastEnd, err
		}
	}
	return lastEnd, nil
}
