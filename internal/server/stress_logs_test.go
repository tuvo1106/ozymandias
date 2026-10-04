package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// L6: eight appenders posting to /v1/logs, searchers and tails running the
// whole time, under -race. At the end every log the intake acknowledged is
// found by a search (exactly once), the tails saw logs while it ran, and
// closing them leaves no goroutine behind.
func TestLogs_StressAppendersSearchersAndTails(t *testing.T) {
	testutil.CheckGoroutines(t)
	srv, err := New(testConfig(t), Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer func() { ts.Close(); _ = srv.Close() }()
	client := &http.Client{}
	defer client.CloseIdleConnections()

	const appenders, batches, perBatch = 8, 25, 20
	var acked atomic.Int64
	var wg sync.WaitGroup
	for a := 0; a < appenders; a++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := 0; b < batches; b++ {
				var logs []wire.Log
				for i := 0; i < perBatch; i++ {
					status := "info"
					if i%5 == 0 {
						status = "error"
					}
					logs = append(logs, wire.Log{
						Ts: time.Now().UnixMilli(), Message: fmt.Sprintf("a%d b%d i%d", a, b, i), Status: status,
						Service: fmt.Sprintf("svc-%d", a%3), Attrs: map[string]any{"appender": a},
					})
				}
				body, _ := json.Marshal(wire.LogsPayload{Logs: logs})
				resp, err := client.Post(ts.URL+"/v1/logs", "application/json", bytes.NewReader(body))
				if err != nil {
					t.Errorf("post: %v", err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusAccepted {
					acked.Add(perBatch)
				} else {
					t.Errorf("status %d", resp.StatusCode)
				}
			}
		}()
	}

	stop := make(chan struct{})
	var bg sync.WaitGroup
	for s := 0; s < 2; s++ {
		bg.Add(1)
		go func() {
			defer bg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				q := url.Values{"q": {"status:error"}, "limit": {"50"}, "from": {"0"}, "to": {fmt.Sprint(time.Now().Add(time.Hour).UnixMilli())}}
				resp, err := client.Get(ts.URL + "/api/v1/logs?" + q.Encode())
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
			}
		}()
	}
	var tailed atomic.Int64
	for s := 0; s < 3; s++ {
		bg.Add(1)
		go func() {
			defer bg.Done()
			req, _ := http.NewRequest("GET", ts.URL+"/api/v1/logs/tail?q="+url.QueryEscape("status:error"), nil)
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			br := bufio.NewReader(resp.Body)
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.HasPrefix(line, "event: log") {
					tailed.Add(1)
				}
			}
		}()
	}

	wg.Wait()
	// Every acknowledged log is searchable, once.
	deadline := time.Now().Add(10 * time.Second)
	var seen map[string]int
	for {
		seen = map[string]int{}
		cursor := ""
		for {
			q := url.Values{"limit": {"1000"}, "from": {"0"}, "to": {fmt.Sprint(time.Now().Add(time.Hour).UnixMilli())}, "cursor": {cursor}}
			resp, err := client.Get(ts.URL + "/api/v1/logs?" + q.Encode())
			if err != nil {
				t.Fatal(err)
			}
			var page struct {
				Logs   []wire.Log `json:"logs"`
				Cursor string     `json:"cursor"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&page)
			_ = resp.Body.Close()
			for _, l := range page.Logs {
				seen[l.Message]++
			}
			if page.Cursor == "" {
				break
			}
			cursor = page.Cursor
		}
		if int64(len(seen)) == acked.Load() || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if int64(len(seen)) != acked.Load() || acked.Load() != appenders*batches*perBatch {
		t.Fatalf("%d distinct logs found, %d acknowledged (want %d)", len(seen), acked.Load(), appenders*batches*perBatch)
	}
	for msg, n := range seen {
		if n != 1 {
			t.Fatalf("%q was returned %d times", msg, n)
		}
	}
	testutil.Eventually(t, 5*time.Second, func() bool { return tailed.Load() > 0 }, "no tail ever saw a log")
	close(stop)
	ts.CloseClientConnections() // ends the tails' streams
	bg.Wait()
	testutil.Eventually(t, 5*time.Second, func() bool { return srv.logHub.Stats().Subscribers == 0 }, "tail subscriptions not released")
}

// Silent emptiness: a metric that is registered but never appears reads as
// "nothing is wrong". The log store's self-metrics must be in /debug/vars.
func TestLogs_StoreSelfMetricsAreReported(t *testing.T) {
	srv, err := New(testConfig(t), Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	body, _ := json.Marshal(wire.LogsPayload{Logs: []wire.Log{{Ts: time.Now().UnixMilli(), Message: "x", Status: "info", Service: "s"}}})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/logs", bytes.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/debug/vars", nil))
	for _, name := range []string{"streams", "chunks", "entries", "head_entries", "head_bytes", "raw_bytes", "compressed_bytes", "bloom_bytes"} {
		if !strings.Contains(rec.Body.String(), `"ozy.logstore.`+name+`"`) {
			t.Errorf("ozy.logstore.%s is not in /debug/vars", name)
		}
	}
	if !strings.Contains(rec.Body.String(), `"ozy.logstore.head_entries","type":"gauge","value":1`) && !regexpHeadOne(rec.Body.String()) {
		t.Errorf("head_entries did not count the log: %s", rec.Body)
	}
}

func regexpHeadOne(s string) bool {
	i := strings.Index(s, `"ozy.logstore.head_entries"`)
	if i < 0 {
		return false
	}
	j := strings.Index(s[i:], `"value":`)
	return j >= 0 && strings.HasPrefix(s[i+j:], `"value":1`)
}
