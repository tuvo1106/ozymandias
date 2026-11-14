package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/config"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

func queuedConfig(t *testing.T) config.Ozyd {
	cfg := testConfig(t)
	cfg.Pipeline.Mode = config.PipelineQueued
	// Negative: no batching window. The group-commit timer runs on the server clock, which these tests fake.
	cfg.Pipeline.Queue.SyncInterval = -1
	return cfg
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, bytes.NewBufferString(body)))
	return rec
}

func seriesCount(t *testing.T, h http.Handler, metric string) int {
	t.Helper()
	rec := get(t, h, "/api/v1/query?metric="+metric+"&agg=sum&from=1790000000&to=1790000009&interval=10")
	var out struct {
		Series []struct {
			Points [][2]any `json:"points"`
		} `json:"series"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	n := 0
	for _, s := range out.Series {
		for _, p := range s.Points {
			if p[1] != nil {
				n++
			}
		}
	}
	return n
}

// Intake answers once the request is durable in the queue; the consumers feed
// the stores. A restart in between loses nothing: what was queued while no
// consumer ran is applied by the next process.
func TestQueued_AcceptedDataSurvivesARestartBeforeItIsStored(t *testing.T) {
	cfg := queuedConfig(t)
	clk := testutil.NewFakeClock(time.Unix(1790000000, 0))
	s, err := New(cfg, Options{Logger: quiet, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	// Not running: no consumer, so this is "acknowledged, not yet stored".
	body := `{"series":[{"metric":"q.count","type":"count","interval":10,"tags":["env:t"],"points":[[1790000000,3]]}]}`
	if rec := post(t, s.Handler(), "/v1/series", body); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	logBody := `{"logs":[{"ts":1790000000000,"message":"queued line","status":"info","service":"svc"}]}`
	if rec := post(t, s.Handler(), "/v1/logs", logBody); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = New(cfg, Options{Logger: quiet, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, mustListen(t)) }()
	defer func() { cancel(); <-done }()

	testutil.Eventually(t, 10*time.Second, func() bool {
		return seriesCount(t, s.Handler(), "q.count") == 1
	}, "the series queued before the restart was never stored")
	testutil.Eventually(t, 10*time.Second, func() bool {
		rec := get(t, s.Handler(), "/api/v1/logs?q=service:svc")
		return bytes.Contains(rec.Body.Bytes(), []byte("queued line"))
	}, "the log queued before the restart was never stored")
}

func mustListen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := httpserve.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// Silent emptiness: lag is the signal an operator watches, and it reads zero
// both when the consumer is caught up and when nothing reports it. Accepted
// data with no consumer running must show as lag, per topic.
func TestQueued_LagIsReportedPerTopic(t *testing.T) {
	s, err := New(queuedConfig(t), Options{Logger: quiet, Clock: testutil.NewFakeClock(time.Unix(1790000000, 0))})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	body := `{"series":[{"metric":"q.count","type":"count","interval":10,"tags":[],"points":[[1790000000,3]]}]}`
	if rec := post(t, s.Handler(), "/v1/series", body); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var vars struct {
		Metrics []struct {
			Name  string   `json:"name"`
			Tags  []string `json:"tags"`
			Value float64  `json:"value"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(get(t, s.Handler(), "/debug/vars").Body.Bytes(), &vars); err != nil {
		t.Fatal(err)
	}
	lag := map[string]float64{}
	present := map[string]bool{}
	for _, m := range vars.Metrics {
		present[m.Name] = true
		if m.Name == "ozy.queue.lag_records" && len(m.Tags) == 1 {
			lag[m.Tags[0]] = m.Value
		}
	}
	for _, name := range []string{"series_stored", "series_rejected", "points_stored", "sketch_points_stored",
		"logs_stored", "logs_rejected", "spans_stored", "spans_rejected"} {
		if !present["ozy.consumer."+name] {
			t.Errorf("ozy.consumer.%s is not in /debug/vars: a counter that is never registered reads as health", name)
		}
	}
	want := map[string]float64{"topic:series": 1, "topic:sketches": 0, "topic:logs": 0, "topic:traces": 0}
	for k, v := range want {
		if got, ok := lag[k]; !ok || got != v {
			t.Errorf("lag_records{%s} = %v (present %v), want %v", k, got, ok, v)
		}
	}
}

// Silent emptiness: the load-control signals must exist before they fire.
func TestQueued_ShedAndQueryLimitMetricsAreRegisteredUpFront(t *testing.T) {
	s, err := New(queuedConfig(t), Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	body := get(t, s.Handler(), "/debug/vars").Body.String()
	for _, name := range []string{"ozy.query.in_flight", "ozy.query.queued", "ozy.query.rejected", "ozy.intake.shed"} {
		if !bytes.Contains([]byte(body), []byte(`"`+name+`"`)) {
			t.Errorf("%s is not in /debug/vars", name)
		}
	}
}
