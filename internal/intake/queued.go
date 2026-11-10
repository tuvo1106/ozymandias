package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/tuvo1106/ozymandias/internal/queue"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/sketchstore"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Topic names, one per signal. They are also the directory names under the
// queue root and the `topic` tag on the lag metrics.
const (
	TopicSeries   = "series"
	TopicSketches = "sketches"
	TopicLogs     = "logs"
	TopicTraces   = "traces"
)

// Topics lists every topic in the order the stores are drained after an
// outage: the cheapest and most valuable signal first (M7 §2).
var Topics = [...]string{TopicSeries, TopicSketches, TopicLogs, TopicTraces}

// Producer is the write side of the queue: Produce returns once the payload is
// durable. In production it is a *queue.Topic per signal.
type Producer interface {
	Produce(ctx context.Context, payload []byte) (uint64, error)
}

// retryAfter is what a sender is told to wait when the queue refuses. Fixed
// rather than computed: the queue cannot know when a consumer will catch up,
// and a constant is something a client can reason about.
const retryAfter = 5 * time.Second

// enqueue is the queued-mode tail of a handler. The payload is the validated,
// decoded-and-re-encoded request, as JSON: self-describing, so a record can be
// read with `jq` after a crash and a format change is a code change rather
// than a migration. The cost is size and CPU against a binary encoding, and
// the measured numbers are in docs/notes/M7.md.
//
// It reports whether the record is durable; if not it has already answered.
func (in *Intake) enqueue(w http.ResponseWriter, r *http.Request, topic string, v any) bool {
	payload, err := json.Marshal(v)
	if err != nil {
		in.opts.Logger.Error("intake: encoding queue record", "topic", topic, "err", err)
		writeError(w, http.StatusInternalServerError, "encoding the request")
		return false
	}
	if _, err := in.opts.Queue[topic].Produce(r.Context(), payload); err != nil {
		switch {
		case errors.Is(err, queue.ErrFull):
			// Not a failure to retry blindly: the queue is full of data we
			// already acknowledged and will not drop. Tell the sender to back off.
			w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter/time.Second)))
			writeError(w, http.StatusTooManyRequests, "the "+topic+" queue is full")
		case errors.Is(err, queue.ErrClosed), errors.Is(err, context.Canceled):
			writeError(w, http.StatusServiceUnavailable, "the server is shutting down")
		default:
			in.opts.Logger.Error("intake: queueing", "topic", topic, "err", err)
			writeError(w, http.StatusServiceUnavailable, "the "+topic+" queue is unavailable")
		}
		return false
	}
	return true
}

// queued reports whether handlers hand off to the queue.
func (in *Intake) queued() bool { return in.opts.Queue != nil }

// queuedResponse is the answer for a request that is durable but not yet
// stored. Accepted means "will be applied", not "is queryable"; what the
// stores later refuse (a point too old for the TSDB, say) cannot be reported
// to a sender that has already been answered, and is counted by the consumer
// instead (docs/adr/0056).
func queuedResponse(accepted int, errs []string) wire.IntakeResponse {
	resp := wire.IntakeResponse{Status: "ok", Accepted: accepted, Rejected: len(errs), Errors: errs}
	if len(resp.Errors) > wire.MaxResponseErrors {
		resp.Errors = resp.Errors[:wire.MaxResponseErrors]
	}
	if resp.Errors == nil {
		resp.Errors = []string{}
	}
	return resp
}

// consumer returns an Intake whose counters are the consumers' own. The same
// store code runs on both sides of the queue, but "accepted by intake" and
// "stored by a consumer" are different events and must not share a series: in
// queued mode they are seconds apart, and after a crash they differ by the
// records redelivered.
func (in *Intake) consumer() *Intake { return in.cons }

// newConsumer registers the consumer counters only when there is a queue:
// in direct mode nothing consumes, and a counter that can never move would
// sit in /debug/vars as a zero that reads like health. (Apply still works
// there, for tests, on counters nobody reads.)
func (in *Intake) newConsumer() *Intake {
	reg := in.opts.Metrics
	if in.opts.Queue == nil {
		reg = selfmetrics.NewRegistry()
	}
	return &Intake{
		opts:          in.opts,
		accepted:      reg.Counter("ozy.consumer.series_stored"),
		rejected:      reg.Counter("ozy.consumer.series_rejected"),
		points:        reg.Counter("ozy.consumer.points_stored"),
		sketchPoints:  reg.Counter("ozy.consumer.sketch_points_stored"),
		logsAccepted:  reg.Counter("ozy.consumer.logs_stored"),
		logsRejected:  reg.Counter("ozy.consumer.logs_rejected"),
		spansAccepted: reg.Counter("ozy.consumer.spans_stored"),
		spansRejected: reg.Counter("ozy.consumer.spans_rejected"),
	}
}

// errPoison marks a record that no retry can apply: it decoded to nothing the
// stores accept. Retrying it would stall the topic behind it for ever, so the
// consumer counts it, logs it and commits past it.
var errPoison = errors.New("undecodable queue record")

// Apply applies one record to its store. A nil return means the record is
// done (stored, or poison and counted) and may be committed; any other error
// is a store failure to retry. Applying is idempotent per store — the
// property that makes at-least-once delivery safe:
//
//   - series: the TSDB no-ops a repeated (t, v);
//   - sketches: the sketch store is last-write-wins by bucket, the scalar
//     series are TSDB series;
//   - logs: the log store drops an entry it holds already (logstore.Append);
//   - traces: the trace store overwrites a span by key.
func (in *Intake) Apply(ctx context.Context, topic string, payload []byte) error {
	c := in.consumer()
	switch topic {
	case TopicSeries:
		var v []wire.Series
		if err := json.Unmarshal(payload, &v); err != nil {
			return fmt.Errorf("%w: %w", errPoison, err)
		}
		res, err := c.storeSeries(ctx, v, nil)
		in.noteRejects(topic, res)
		return err
	case TopicSketches:
		var v []wire.SketchSeries
		if err := json.Unmarshal(payload, &v); err != nil {
			return fmt.Errorf("%w: %w", errPoison, err)
		}
		decoded := make([][]sketchstore.Point, 0, len(v))
		kept := v[:0]
		for _, s := range v {
			points, reason := decodeSketchPoints(s)
			if reason != "" { // validated at intake; reaching here means the record changed
				in.opts.Logger.Error("consumer: sketch no longer decodes", "metric", s.Metric, "reason", reason)
				continue
			}
			kept = append(kept, s)
			decoded = append(decoded, points)
		}
		res, err := c.storeSketches(ctx, kept, decoded, nil)
		in.noteRejects(topic, res)
		return err
	case TopicLogs:
		var v []wire.Log
		if err := json.Unmarshal(payload, &v); err != nil {
			return fmt.Errorf("%w: %w", errPoison, err)
		}
		if err := in.opts.Logs.Append(ctx, v); err != nil {
			return err
		}
		// After the store, as in direct mode: a tail never shows a log a crash
		// could lose. A redelivery publishes again; live tail is best effort
		// and a repeat line there is the price of not tracking offsets in it.
		if in.opts.LogHub != nil {
			in.opts.LogHub.Publish(v)
		}
		c.logsAccepted.Add(int64(len(v)))
		return nil
	case TopicTraces:
		var v []wire.Span
		if err := json.Unmarshal(payload, &v); err != nil {
			return fmt.Errorf("%w: %w", errPoison, err)
		}
		if err := in.opts.Traces.Append(ctx, v); err != nil {
			return err
		}
		c.spansAccepted.Add(int64(len(v)))
		return nil
	}
	return fmt.Errorf("%w: unknown topic %q", errPoison, topic)
}

// noteRejects logs what a store refused after the sender was answered.
func (in *Intake) noteRejects(topic string, resp wire.IntakeResponse) {
	if resp.Rejected > 0 {
		in.opts.Logger.Warn("consumer: store rejected items", "topic", topic,
			"rejected", resp.Rejected, "errors", resp.Errors)
	}
}
