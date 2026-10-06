package intake

import (
	"context"
	"errors"
	"net/http"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// SpanStore is where accepted spans go (implemented by internal/tracestore).
// Append returns nil only once the spans are durable.
type SpanStore interface {
	Append(ctx context.Context, spans []wire.Span) error
}

// traces handles POST /v1/traces (§F): decode, validate each span, store what
// survives, report per-span outcomes. The agent already normalized and sampled,
// but ozyd does not trust that: anything can post here.
//
// A store error is a 503 with nothing stored, so the agent's retry resends the
// whole batch; and the store's writes are idempotent for that reason.
func (in *Intake) traces(w http.ResponseWriter, r *http.Request) {
	data, err := readBody(w, r)
	if errors.Is(err, errTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.opts.Traces == nil {
		writeError(w, http.StatusServiceUnavailable, "this server has no trace store")
		return
	}
	p, rejects, err := wire.DecodeSpans(data, wire.DecodeOptions{Now: in.opts.Clock.Now()})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for i := range p.Spans {
		// A sender-level env and host fill in what a span did not say, as the agent does.
		sp := &p.Spans[i]
		if sp.Meta == nil {
			sp.Meta = map[string]string{}
		}
		if sp.Meta["env"] == "" && p.Env != "" {
			sp.Meta["env"] = p.Env
		}
		if sp.Meta["host"] == "" && p.Host != "" {
			sp.Meta["host"] = p.Host
		}
	}
	errs := make([]string, 0, min(len(rejects), wire.MaxResponseErrors))
	for i, rj := range rejects {
		if i == wire.MaxResponseErrors {
			break
		}
		errs = append(errs, rj.String())
	}
	if len(p.Spans) > 0 {
		if err := in.opts.Traces.Append(r.Context(), p.Spans); err != nil {
			in.opts.Logger.Error("intake: storing spans", "err", err, "spans", len(p.Spans))
			writeError(w, http.StatusServiceUnavailable, "storing spans: "+err.Error())
			return
		}
	}
	in.spansAccepted.Add(int64(len(p.Spans)))
	in.spansRejected.Add(int64(len(rejects)))
	writeJSON(w, http.StatusAccepted, wire.IntakeResponse{Status: "ok", Accepted: len(p.Spans), Rejected: len(rejects), Errors: errs})
}
