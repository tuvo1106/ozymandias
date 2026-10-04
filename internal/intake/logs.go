package intake

import (
	"context"
	"errors"
	"net/http"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// LogStore is where accepted logs go (implemented by internal/logstore). Append
// returns nil only once the batch is durable: the agent commits its file offsets
// on this answer.
type LogStore interface {
	Append(ctx context.Context, logs []wire.Log) error
}

// LogPublisher is told about every log after it is stored, for live tail
// (implemented by internal/loghub). It must not block.
type LogPublisher interface {
	Publish(logs []wire.Log)
}

// logs handles POST /v1/logs (§E): decode, validate each log, store what
// survives, publish it to live tail, and report per-log outcomes.
//
// Order matters twice. The store comes before the publish, so a tail never
// shows a log that a crash could then lose. And a store error is a 503 with
// nothing published: the agent will resend the whole batch, and publishing now
// would show it twice.
func (in *Intake) logs(w http.ResponseWriter, r *http.Request) {
	data, err := readBody(w, r)
	if errors.Is(err, errTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.opts.Logs == nil {
		writeError(w, http.StatusServiceUnavailable, "this server has no log store")
		return
	}
	valid, rejects, err := wire.DecodeLogs(data, wire.DecodeOptions{Now: in.opts.Clock.Now()})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	errs := make([]string, 0, min(len(rejects), wire.MaxResponseErrors))
	for i, rj := range rejects {
		if i == wire.MaxResponseErrors {
			break
		}
		errs = append(errs, rj.String())
	}
	if len(valid) > 0 {
		if err := in.opts.Logs.Append(r.Context(), valid); err != nil {
			in.opts.Logger.Error("intake: storing logs", "err", err, "logs", len(valid))
			writeError(w, http.StatusServiceUnavailable, "storing logs: "+err.Error())
			return
		}
		if in.opts.LogHub != nil {
			in.opts.LogHub.Publish(valid)
		}
	}
	in.logsAccepted.Add(int64(len(valid)))
	in.logsRejected.Add(int64(len(rejects)))
	writeJSON(w, http.StatusAccepted, wire.IntakeResponse{Status: "ok", Accepted: len(valid), Rejected: len(rejects), Errors: errs})
}
