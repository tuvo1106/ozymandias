package httpserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/tuvo1106/ozymandias/internal/buildinfo"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
)

// Serve runs srv on ln until ctx is cancelled or the server fails. On
// cancellation it shuts down gracefully: new connections are refused at once,
// in-flight requests get up to grace to complete, and anything still running
// after that is forcibly closed.
//
// It returns nil after a clean shutdown, the server's error if it failed on
// its own, or an error if the grace period ran out.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener, grace time.Duration) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		// The server stopped without being asked: a listener failure.
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	// ctx is already done, so shutdown needs a fresh deadline of its own.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	<-errc // Serve has returned ErrServerClosed; wait so no goroutine outlives us
	if err != nil {
		_ = srv.Close()
		return fmt.Errorf("graceful shutdown did not finish within %v: %w", grace, err)
	}
	return nil
}

// Instrument wraps h so every request increments the
// `ozy.http.requests` counter, tagged with the component, the matched
// route pattern (never the raw path: that would be one series per URL) and
// the status class (2xx, 4xx, …).
func Instrument(reg *selfmetrics.Registry, component string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, r)
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		reg.Counter("ozy.http.requests",
			"component:"+component,
			"route:"+route,
			fmt.Sprintf("status_class:%dxx", sw.status/100),
		).Inc()
	})
}

// statusWriter records the status code a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer (for
// flushing, deadlines) through this wrapper.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// LoopbackURL turns a listen address into a URL for reaching that listener
// from the same host: ":9400" and "0.0.0.0:9400" become
// "http://127.0.0.1:9400"; a specific host is kept. Used by the healthcheck
// subcommand, which runs inside the container it checks.
func LoopbackURL(addr, path string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("listen address %q: %w", addr, err)
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + path, nil
}

// Probe GETs url and returns nil if it answers 2xx within timeout. The body is
// included in the error otherwise, since a failing /healthz explains itself.
func Probe(ctx context.Context, url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// ErrNotListening is returned by Listen when the address can't be bound.
var ErrNotListening = errors.New("cannot listen")

// Listen opens a TCP listener on addr, wrapping the error so callers can tell
// "port in use" from other startup failures.
func Listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w on %s: %w", ErrNotListening, addr, err)
	}
	return ln, nil
}

// Health returns the /healthz handler shared by both binaries. It always
// answers 200 while the process is serving — it is a liveness check, not a
// readiness check (M7 adds /readyz for "stores open, WAL replayed").
// extra adds component-specific fields; it is called per request.
func Health(component string, started time.Time, now func() time.Time, extra func() map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{
			"status":         "ok",
			"component":      component,
			"version":        buildinfo.Version,
			"uptime_seconds": math.Round(now().Sub(started).Seconds()),
		}
		if extra != nil {
			for k, v := range extra() {
				body[k] = v
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
}
