package intake

import (
	"bytes"
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

// Shed thresholds, as a fraction of a topic's own cap (M7 §2). Each topic is
// judged by its own fullness, so this is an early warning that is looser for
// metrics, not a cross-signal priority: a backed-up logs topic does not make
// room in the series topic. Logs and traces are the expensive signals and the
// ones whose loss hurts least, so they are refused at 80%; metrics are the
// cheapest per byte and the most valuable, so they hold on until 95%.
const (
	ShedExpensiveAt = 0.80
	ShedCheapAt     = 0.95
)

// shedAt is the fullness at which a topic starts refusing.
func shedAt(topic string) float64 {
	if topic == TopicSeries || topic == TopicSketches {
		return ShedCheapAt
	}
	return ShedExpensiveAt
}

// Fuller is implemented by producers that can say how full they are.
type Fuller interface{ Fullness() float64 }

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
	prod := in.opts.Queue[topic]
	if prod == nil {
		in.opts.Logger.Error("intake: no queue for topic", "topic", topic)
		writeError(w, http.StatusServiceUnavailable, "the "+topic+" queue is not configured")
		return false
	}
	if _, err := prod.Produce(r.Context(), payload); err != nil {
		switch {
		case errors.Is(err, queue.ErrFull):
			// Not a failure to retry blindly: the queue is full of data we
			// already acknowledged and will not drop. Tell the sender to back off.
			w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter/time.Second)))
			writeError(w, http.StatusTooManyRequests, "the "+topic+" queue is full")
		case errors.Is(err, queue.ErrClosed):
			writeError(w, http.StatusServiceUnavailable, "the server is shutting down")
		case errors.Is(err, context.Canceled):
			// The sender went away. Not counted as a fault; if the record became
			// durable first, its retry is a redelivery the stores absorb.
			writeError(w, http.StatusServiceUnavailable, "request cancelled")
		default:
			in.opts.Logger.Error("intake: queueing", "topic", topic, "err", err)
			writeError(w, http.StatusServiceUnavailable, "the "+topic+" queue is unavailable")
		}
		return false
	}
	return true
}

// shed counts requests refused ahead of the cap, per topic.
func (in *Intake) shed(topic string) *selfmetrics.Counter {
	return in.opts.Metrics.Counter("ozy.intake.shed", "topic:"+topic)
}

// shedding refuses the request if its topic is past its shed threshold. It
// runs first in each handler, before the body is read: a shed request must cost
// as little as possible (no decode, no registry write for a series that is then
// refused), or shedding saves the queue write and nothing else.
func (in *Intake) shedding(w http.ResponseWriter, topic string) bool {
	f, ok := in.opts.Queue[topic].(Fuller)
	if !ok || f.Fullness() < shedAt(topic) {
		return false
	}
	in.shed(topic).Add(1)
	w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter/time.Second)))
	writeError(w, http.StatusTooManyRequests, "shedding "+topic+": the queue is nearly full")
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
		// UseNumber, as wire.DecodeLogs does: attrs must keep numbers exact
		// (ids above 2^53, 1.50 vs 1.5), or the two modes store different bytes.
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
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
