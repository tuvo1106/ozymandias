package dockerapi

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDaemon serves handler on a unix socket, the way dockerd does, and
// returns a Client pointed at it. The socket lives in a short temp dir:
// macOS caps a unix socket path at 104 bytes, and t.TempDir's paths,
// which include the test name, can exceed that.
func fakeDaemon(t *testing.T, handler http.Handler, opts Options) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "dk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	opts.Socket = sock
	c := New(opts)
	t.Cleanup(c.Close)
	return c
}

func serveFile(t *testing.T, name string) http.HandlerFunc {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}
}

const apiID = "8dfafdbc3a40c7b5f8e2a9c4d1e6b0a3f7c2d9e8b1a4c6f0e3d7b2a9c5e8f1d4"

func TestListContainers(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("GET /containers/json", serveFile(t, "containers.json"))
	c := fakeDaemon(t, mux, Options{})
	got, err := c.ListContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d containers, want 3", len(got))
	}
	api := got[0]
	if api.ID != apiID || api.Name() != "shop-api-1" || api.Image != "shop/api:1.4.2" ||
		api.Labels["com.docker.compose.service"] != "api" || api.Labels["ozy.service"] != "shop-api" {
		t.Errorf("first container decoded as %+v", api)
	}
	// No names at all: the short id stands in, never "".
	if n := got[2].Name(); n != "f0e1d2c3b4a5" {
		t.Errorf("nameless container: Name() = %q", n)
	}
	if got[2].Labels != nil {
		t.Errorf("null labels decoded as %v", got[2].Labels)
	}
}

func TestStats_RequestsOneSnapshot(t *testing.T) {
	var path, stream string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/{id}/stats", func(w http.ResponseWriter, r *http.Request) {
		path, stream = r.URL.Path, r.URL.Query().Get("stream")
		serveFile(t, "stats-cgroupv2.json")(w, r)
	})
	c := fakeDaemon(t, mux, Options{})
	s, err := c.Stats(context.Background(), apiID)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/containers/"+apiID+"/stats" || stream != "false" {
		t.Errorf("requested %s stream=%q; want one snapshot, not a stream", path, stream)
	}
	if !s.Sampled() || s.MemoryStats.Limit != 536870912 || s.CPUStats.OnlineCPUs != 4 {
		t.Errorf("decoded %+v", s)
	}
}

func TestStatsAndInspect_ContainerGone(t *testing.T) {
	// The normal race: a container exits between the list and the stats
	// call. The collector must be able to tell that from a real failure.
	mux := http.NewServeMux()
	gone := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"message":"No such container: %s"}`, r.PathValue("id"))
	}
	mux.HandleFunc("GET /containers/{id}/stats", gone)
	mux.HandleFunc("GET /containers/{id}/json", gone)
	c := fakeDaemon(t, mux, Options{})

	_, err := c.Stats(context.Background(), "deadbeef")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("stats: err = %v, want ErrNotFound", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "No such container: deadbeef" || apiErr.Status != 404 {
		t.Errorf("stats: error detail %+v", apiErr)
	}
	if _, err := c.Inspect(context.Background(), "deadbeef"); !errors.Is(err, ErrNotFound) {
		t.Errorf("inspect: err = %v, want ErrNotFound", err)
	}
}

// The same race, seen live: a container stopping while the daemon takes
// its second CPU sample gets a 200 with no body. Stats reports it as
// unsampled; Inspect, which has no such race, still calls it an error.
func TestStats_EmptyBodyIsUnsampled(t *testing.T) {
	mux := http.NewServeMux()
	empty := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("GET /containers/{id}/stats", empty)
	mux.HandleFunc("GET /containers/{id}/json", empty)
	c := fakeDaemon(t, mux, Options{})
	s, err := c.Stats(context.Background(), apiID)
	if err != nil || s.Sampled() {
		t.Fatalf("stats = %+v, %v; want unsampled and no error", s, err)
	}
	if _, err := c.Inspect(context.Background(), apiID); err == nil {
		t.Error("inspect of an empty body: no error")
	}
}

func TestInspect(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("GET /containers/{id}/json", serveFile(t, "inspect-exited.json"))
	c := fakeDaemon(t, mux, Options{})
	got, err := c.Inspect(context.Background(), "b7c1d9e3f5a2")
	if err != nil {
		t.Fatal(err)
	}
	if st := got.State; !st.OOMKilled || st.StartedAt.IsZero() {
		t.Errorf("state %+v", st)
	}
	if got.Config.Image != "judge-python:3.12" {
		t.Errorf("Config.Image = %q", got.Config.Image)
	}
}

func TestClient_Failures(t *testing.T) {
	ctx := context.Background()
	t.Run("daemon down", func(t *testing.T) {
		c := New(Options{Socket: filepath.Join(os.TempDir(), "no-such-docker.sock")})
		if _, err := c.ListContainers(ctx); err == nil || !strings.Contains(err.Error(), "/containers/json") {
			t.Fatalf("err = %v, want a dial error naming the call", err)
		}
	})
	t.Run("slow daemon", func(t *testing.T) {
		release := make(chan struct{})
		c := fakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}), Options{Timeout: 50 * time.Millisecond})
		defer close(release)
		start := time.Now()
		_, err := c.Stats(ctx, apiID)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the request deadline", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("took %v; the timeout did not bound the call", d)
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		c := fakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[{"Id": "abc", "Names": [`))
		}), Options{})
		if _, err := c.ListContainers(ctx); err == nil || !strings.Contains(err.Error(), "decoding") {
			t.Fatalf("err = %v, want a decoding error", err)
		}
	})
	t.Run("oversized body", func(t *testing.T) {
		c := fakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte(" "), 2048))
		}), Options{MaxBodyBytes: 1024})
		if _, err := c.ListContainers(ctx); err == nil || !strings.Contains(err.Error(), "larger than 1024") {
			t.Fatalf("err = %v, want the size bound", err)
		}
	})
	t.Run("server error without JSON", func(t *testing.T) {
		c := fakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "daemon is shutting down", http.StatusServiceUnavailable)
		}), Options{})
		_, err := c.ListContainers(ctx)
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 503 || apiErr.Message != "daemon is shutting down" || errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("server error with no body", func(t *testing.T) {
		c := fakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}), Options{})
		_, err := c.ListContainers(ctx)
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Message != "Internal Server Error" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("empty id", func(t *testing.T) {
		c := New(Options{})
		if _, err := c.Stats(ctx, ""); err == nil {
			t.Error("stats with an empty id: want error")
		}
		if _, err := c.Inspect(ctx, ""); err == nil {
			t.Error("inspect with an empty id: want error")
		}
	})
}

// eventLines is the fixture as the daemon would stream it.
func eventLines(t *testing.T) []string {
	b, err := os.ReadFile("testdata/events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestEvents_StreamThenDaemonCloses(t *testing.T) {
	var filters string
	lines := eventLines(t)
	c := fakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filters = r.URL.Query().Get("filters")
		w.WriteHeader(http.StatusOK)
		// A blank line between events, as some proxies add: skipped.
		_, _ = fmt.Fprintf(w, "%s\n\n%s\n%s\n", lines[0], lines[1], lines[2])
	}), Options{})
	var got []Event
	err := c.Events(context.Background(), time.Time{}, func(e Event) error { got = append(got, e); return nil }, nil)
	if !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("err = %v, want ErrStreamClosed so the caller reconnects", err)
	}
	if filters != eventFilters {
		t.Errorf("filters = %q; the daemon should do the filtering", filters)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	if got[0].Action != "start" || got[1].Action != "die" {
		t.Errorf("actions %q, %q", got[0].Action, got[1].Action)
	}
	if code, ok := got[1].ExitCode(); !ok || code != 137 {
		t.Errorf("die exit code = %d, %v", code, ok)
	}
	if !got[1].At().Equal(time.Unix(0, 1790701802750000000)) {
		t.Errorf("At = %v, want nanosecond precision", got[1].At())
	}
	// The legacy-only event still comes out complete.
	legacy := got[2]
	if legacy.Action != "die" || legacy.Actor.ID != "0f9e8d7c6b5a4938271605f4e3d2c1b0a9f8e7d6c5b4a3928170f6e5d4c3b2a1" ||
		!legacy.At().Equal(time.Unix(1790701803, 0)) {
		t.Errorf("legacy event %+v", legacy)
	}
	if _, ok := legacy.ExitCode(); ok {
		t.Error("an event without exitCode reported one")
	}
}

func TestEvents_ResumeAfterDisconnect(t *testing.T) {
	// The first connection delivers two events and drops; the caller
	// reconnects with since = the last event's time and gets the rest,
	// starting again from that event (since is inclusive).
	lines := eventLines(t)
	var mu sync.Mutex
	var sinces []string
	c := fakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		since := r.URL.Query().Get("since")
		sinces = append(sinces, since)
		mu.Unlock()
		if since == "" {
			_, _ = fmt.Fprintf(w, "%s\n%s\n", lines[0], lines[1])
			return
		}
		_, _ = fmt.Fprintf(w, "%s\n%s\n", lines[1], lines[2])
	}), Options{})

	var got []Event
	var since time.Time
	for attempt := 0; attempt < 2; attempt++ {
		err := c.Events(context.Background(), since, func(e Event) error {
			got = append(got, e)
			since = e.At()
			return nil
		}, nil)
		if !errors.Is(err, ErrStreamClosed) {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	if len(sinces) != 2 || sinces[0] != "" || sinces[1] != "1790701802.750000000" {
		t.Fatalf("since parameters %q; want none, then the last event's time to the nanosecond", sinces)
	}
	if len(got) != 4 || got[1].At() != got[2].At() {
		t.Fatalf("got %d events; want the resume point redelivered once", len(got))
	}
}

func TestEvents_Stops(t *testing.T) {
	lines := eventLines(t)
	// A stream that stays open after one event, like a quiet daemon.
	open := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, lines[0])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	t.Run("context cancelled", func(t *testing.T) {
		c := fakeDaemon(t, open, Options{})
		ctx, cancel := context.WithCancel(context.Background())
		err := c.Events(ctx, time.Time{}, func(Event) error { cancel(); return nil }, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("callback error", func(t *testing.T) {
		c := fakeDaemon(t, open, Options{})
		stop := errors.New("stop")
		if err := c.Events(context.Background(), time.Time{}, func(Event) error { return stop }, nil); !errors.Is(err, stop) {
			t.Fatalf("err = %v, want the callback's error", err)
		}
	})
	// A bad line is skipped and reported, and the events after it still
	// arrive: ending the stream would reconnect, and the daemon would
	// replay the same line forever.
	good := `{"Type":"container","Action":"die","Actor":{"ID":"` + apiID + `"},"time":1790000000,"timeNano":1790000000000000000}`
	for name, bad := range map[string]func(w *bufio.Writer){
		"malformed event": func(w *bufio.Writer) { _, _ = w.WriteString(`{"Type":"container","Action":` + "\n") },
		"oversized event": func(w *bufio.Writer) {
			_, _ = w.WriteString(`{"Actor":{"Attributes":{"x":"`)
			_, _ = w.Write(bytes.Repeat([]byte("a"), MaxEventBytes+1))
			_, _ = w.WriteString("\"}}}\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := fakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				bw := bufio.NewWriter(w)
				bad(bw)
				_, _ = bw.WriteString(good + "\n")
				_ = bw.Flush()
			}), Options{})
			var got []Event
			var skipped []error
			err := c.Events(context.Background(), time.Time{}, func(e Event) error { got = append(got, e); return nil },
				func(err error) { skipped = append(skipped, err) })
			if !errors.Is(err, ErrStreamClosed) || len(skipped) != 1 || len(got) != 1 || got[0].Action != "die" {
				t.Fatalf("err %v, skipped %v, got %+v", err, skipped, got)
			}
		})
	}
	t.Run("daemon refuses", func(t *testing.T) {
		c := fakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"invalid filter"}`))
		}), Options{})
		var apiErr *APIError
		err := c.Events(context.Background(), time.Time{}, func(Event) error { return nil }, nil)
		if !errors.As(err, &apiErr) || apiErr.Message != "invalid filter" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("daemon down", func(t *testing.T) {
		c := New(Options{Socket: filepath.Join(os.TempDir(), "no-such-docker.sock")})
		if err := c.Events(context.Background(), time.Time{}, func(Event) error { return nil }, nil); err == nil {
			t.Fatal("want a dial error")
		}
	})
}

func TestFormatSince(t *testing.T) {
	if got := formatSince(time.Unix(1790701802, 5)); got != "1790701802.000000005" {
		t.Errorf("formatSince = %q; the fraction must keep its leading zeros", got)
	}
}

// The idle pool is as large as the caller's concurrency, so a run's
// connections are reused rather than re-dialled; the transport-wide cap
// matches, or it would quietly win.
func TestNew_IdleConnectionsFollowTheCaller(t *testing.T) {
	for _, tc := range []struct{ set, want int }{{0, DefaultMaxIdleConns}, {32, 32}} {
		tr := New(Options{MaxIdleConns: tc.set}).http.Transport.(*http.Transport)
		if tr.MaxIdleConns != tc.want || tr.MaxIdleConnsPerHost != tc.want {
			t.Errorf("MaxIdleConns %d: transport keeps %d / %d per host, want %d", tc.set, tr.MaxIdleConns, tr.MaxIdleConnsPerHost, tc.want)
		}
	}
}

// A linked container lists its link aliases too, in no promised order; the
// name is the one without a further "/", as the event stream reports it.
func TestContainer_NameSkipsLinkAliases(t *testing.T) {
	for _, tc := range []struct {
		names []string
		want  string
	}{
		{[]string{"/web/db", "/db"}, "db"},
		{[]string{"/db", "/web/db"}, "db"},
		{[]string{"/web/db"}, "web/db"}, // only an alias: better than the id
		{[]string{"/"}, ShortID(apiID)},
	} {
		if got := (Container{ID: apiID, Names: tc.names}).Name(); got != tc.want {
			t.Errorf("%v: Name() = %q, want %q", tc.names, got, tc.want)
		}
	}
}

func TestNow_IsTheDaemonsDate(t *testing.T) {
	want := time.Date(2026, 9, 30, 12, 0, 5, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_ping", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", want.Format(http.TimeFormat))
		_, _ = w.Write([]byte("OK"))
	})
	c := fakeDaemon(t, mux, Options{})
	if got, err := c.Now(context.Background()); err != nil || !got.Equal(want) {
		t.Fatalf("Now = %v, %v; want %v", got, err, want)
	}

	down := fakeDaemon(t, http.NotFoundHandler(), Options{})
	if _, err := down.Now(context.Background()); err == nil {
		t.Error("Now against a 404: no error")
	}
}

// A daemon that accepts the event stream and never answers: the stream has
// no deadline of its own, so the header timeout is what returns it to the
// watcher, which reconnects, instead of waiting until shutdown.
func TestEvents_ADaemonThatNeverAnswers(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	c := fakeDaemon(t, mux, Options{Timeout: 100 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // before the server's Close (cleanups run last-first), or a hung stream holds it
	done := make(chan error, 1)
	go func() { done <- c.Events(ctx, time.Time{}, func(Event) error { return nil }, nil) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Events returned no error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Events still waiting on a daemon that never answers")
	}
}

// A daemon that exits mid-stream cuts the chunked body short. That is the
// stream ending (ErrStreamClosed, retried quietly), not a failure to log.
func TestEvents_ADaemonThatDiesMidStream(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n")
		_, _ = buf.WriteString("5\r\n{\"typ\r\n") // then the daemon is gone: no final chunk
		_ = buf.Flush()
	})
	c := fakeDaemon(t, mux, Options{})
	err := c.Events(context.Background(), time.Time{}, func(Event) error { return nil }, nil)
	if !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("err = %v, want ErrStreamClosed", err)
	}
}

func TestLogs_RequestAndBody(t *testing.T) {
	var got string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
		_, _ = w.Write([]byte("frames"))
	})
	c := fakeDaemon(t, mux, Options{})
	rc, err := c.Logs(context.Background(), apiID, time.Unix(1790000000, 5), true)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "frames" {
		t.Fatalf("body %q", b)
	}
	for _, want := range []string{"follow=1", "stdout=1", "stderr=1", "timestamps=1", "since=1790000000.000000005"} {
		if !strings.Contains(got, want) {
			t.Errorf("query %q lacks %s", got, want)
		}
	}
	rc, _ = c.Logs(context.Background(), apiID, time.Time{}, false)
	_ = rc.Close()
	if strings.Contains(got, "since") || strings.Contains(got, "follow") {
		t.Errorf("a zero since and no follow should send neither: %q", got)
	}
	if _, err := c.Logs(context.Background(), "", time.Time{}, true); err == nil {
		t.Error("empty id accepted")
	}
}

func TestLogs_GoneContainerIsNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/{id}/logs", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
	})
	c := fakeDaemon(t, mux, Options{})
	if _, err := c.Logs(context.Background(), apiID, time.Time{}, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}
