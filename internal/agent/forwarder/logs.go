package forwarder

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// LogSink delivers batches of logs to ozyd (POST /v1/logs), one synchronous
// request per batch. It is the tailer's Sink.
//
// It does not queue and does not retry, unlike [Forwarder], and that is the
// point: the tailer commits a file offset only when Send returns nil, so the
// durable queue is the log file itself, which already is one and is bounded by
// the disk rather than by a memory cap. A second queue here would only add a
// way to lose logs between the two. When ozyd is down Send fails, the tailer
// leaves its offset where it was, and the next poll reads the same lines again.
type LogSink struct {
	url, host, version string
	client             *http.Client

	sent, rejected, refused, failed *selfmetrics.Counter
	log                             *slog.Logger
}

// LogSinkOptions configures a LogSink.
type LogSinkOptions struct {
	// URL is ozyd's base URL.
	URL string
	// Client does the HTTP; default one with Timeout.
	Client  *http.Client
	Timeout time.Duration // default 10s
	// Hostname and Version go in the agent identity headers.
	Hostname, Version string
	Registry          *selfmetrics.Registry
	Logger            *slog.Logger
}

// NewLogSink returns a sink for opts.
func NewLogSink(opts LogSinkOptions) *LogSink {
	if opts.Client == nil {
		if opts.Timeout <= 0 {
			opts.Timeout = 10 * time.Second
		}
		opts.Client = &http.Client{Timeout: opts.Timeout}
	}
	if opts.Registry == nil {
		opts.Registry = selfmetrics.NewRegistry()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &LogSink{
		url: opts.URL + "/v1/logs", host: opts.Hostname, version: opts.Version, client: opts.Client, log: opts.Logger,
		sent:     opts.Registry.Counter("ozy.agent.logs.sent"),
		rejected: opts.Registry.Counter("ozy.agent.logs.rejected"),
		refused:  opts.Registry.Counter("ozy.agent.logs.batches_refused"),
		failed:   opts.Registry.Counter("ozy.agent.logs.send_errors"),
	}
}

// Send posts logs. A nil return means ozyd stored them. Logs ozyd rejected one
// by one (a bad timestamp, say) are counted and dropped: resending cannot fix
// them. A whole batch ozyd refuses as malformed or too large (400, 413) is the
// same, and is dropped rather than retried forever: a poison batch must not
// wedge the file it came from. Everything else (a network error, 429, 5xx) is an
// error, and the batch is sent again.
func (s *LogSink) Send(ctx context.Context, logs []wire.Log) error {
	if len(logs) == 0 {
		return nil
	}
	raw, err := json.Marshal(wire.LogsPayload{Logs: logs})
	if err != nil {
		// A log that cannot be marshalled can never be sent; drop the batch, loudly.
		s.refused.Inc()
		s.log.Error("logs: dropping a batch that cannot be encoded", "err", err, "logs", len(logs))
		return nil
	}
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	_, _ = zw.Write(raw)
	if err := zw.Close(); err != nil {
		return fmt.Errorf("compressing logs: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, &body)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set(wire.HeaderAgentVersion, s.version)
	req.Header.Set(wire.HeaderHost, s.host)
	resp, err := s.client.Do(req)
	if err != nil {
		s.failed.Inc()
		return fmt.Errorf("posting logs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode/100 == 2:
		var r wire.IntakeResponse
		if json.Unmarshal(b, &r) == nil && r.Rejected > 0 {
			s.rejected.Add(int64(r.Rejected))
			s.log.Warn("logs: ozyd rejected some logs", "rejected", r.Rejected, "errors", r.Errors)
		}
		s.sent.Add(int64(len(logs)))
		return nil
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge:
		s.refused.Inc()
		s.log.Error("logs: ozyd refused a batch; dropping it", "status", resp.StatusCode, "logs", len(logs), "body", string(bytes.TrimSpace(b)))
		return nil
	default:
		s.failed.Inc()
		return fmt.Errorf("ozyd answered %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
}
