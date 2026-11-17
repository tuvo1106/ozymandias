package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The bench scenarios are the L12 numbers of docs/plan/testing.md: how long a point takes to
// become queryable, and how much one ozyd takes in. They exist to compare pipeline.mode
// direct with queued on the same machine (scripts/bench/intake.sh), so what they print is
// meant to be read side by side, not as an absolute capacity.
//
// Both scenarios send timestamps three days back, strictly increasing per series: far enough
// that a run can send a hundred thousand distinct timestamps per series without reaching "now"
// (a point in the future is refused), near enough to be inside the logs' retention (168h from
// the end of the day), which would otherwise delete what the run just wrote.

const (
	benchMetric = "bench.intake"
	benchAge    = 72 * time.Hour
)

// percentiles reports p50, p95, p99 and the maximum of d, in milliseconds.
func percentiles(d []time.Duration) (p50, p95, p99, max float64) {
	if len(d) == 0 {
		return
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	at := func(q float64) float64 {
		return float64(d[min(len(d)-1, int(q*float64(len(d))))]) / float64(time.Millisecond)
	}
	return at(0.50), at(0.95), at(0.99), float64(d[len(d)-1]) / float64(time.Millisecond)
}

// intakeLatency sends n single points one at a time and, for each, measures how long ozyd
// takes to answer 202 (ack) and how long until a query returns that point (visible). In
// direct mode the two are the same moment; in queued mode the gap is the queue and a consumer.
func intakeLatency(out io.Writer, ozyd string, n int) error {
	client := &http.Client{Timeout: 10 * time.Second}
	base := time.Now().Add(-benchAge).Unix()
	var acks, visible []time.Duration
	for i := 0; i < n; i++ {
		ts := base + int64(i)
		body := fmt.Sprintf(`{"series":[{"metric":%q,"type":"gauge","interval":0,"tags":["probe:latency"],"points":[[%d,%d]]}]}`, benchMetric, ts, i+1)
		start := time.Now()
		if !post(client, ozyd+"/v1/series", body) {
			return fmt.Errorf("point %d was not acknowledged", i)
		}
		acks = append(acks, time.Since(start))
		q := url.Values{"metric": {benchMetric}, "agg": {"sum"}, "from": {fmt.Sprint(ts)}, "to": {fmt.Sprint(ts + 1)}, "interval": {"1"}}
		deadline := start.Add(30 * time.Second)
		for {
			var res struct {
				Series []struct {
					Points [][]*float64 `json:"points"`
				} `json:"series"`
			}
			if err := getJSON(ozyd+"/api/v1/query?"+q.Encode(), &res); err != nil {
				return err
			}
			seen := false
			for _, s := range res.Series {
				for _, p := range s.Points {
					seen = seen || (len(p) == 2 && p[1] != nil && *p[1] == float64(i+1))
				}
			}
			if seen {
				visible = append(visible, time.Since(start))
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("point %d was acknowledged and still not queryable after 30s", i)
			}
			time.Sleep(time.Millisecond)
		}
	}
	a50, a95, a99, amax := percentiles(acks)
	v50, v95, v99, vmax := percentiles(visible)
	fmt.Fprintf(out, "latency over %d single-point requests, in ms\n", n)
	fmt.Fprintf(out, "  %-22s p50 %7.2f  p95 %7.2f  p99 %7.2f  max %7.2f\n", "acknowledged (202)", a50, a95, a99, amax)
	fmt.Fprintf(out, "  %-22s p50 %7.2f  p95 %7.2f  p99 %7.2f  max %7.2f\n", "queryable", v50, v95, v99, vmax)
	return nil
}

// intakeThroughput sends as fast as `workers` concurrent senders can for the duration and
// reports what ozyd acknowledged per second. signal is "series" (each request carries
// `batch` series of one point) or "logs" (a request carries `batch` log lines).
func intakeThroughput(out io.Writer, ozyd, signal string, workers, batch int, d time.Duration) error {
	if signal != "series" && signal != "logs" {
		return fmt.Errorf("signal %q: want series or logs", signal)
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: workers}}
	base := time.Now().Add(-benchAge).Unix()
	var requests, items, failed atomic.Int64
	var wg sync.WaitGroup
	var latMu sync.Mutex
	var lat []time.Duration
	end := time.Now().Add(d)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var mine []time.Duration
			for round := int64(0); time.Now().Before(end); round++ {
				var sb strings.Builder
				path := "/v1/series"
				if signal == "series" {
					sb.WriteString(`{"series":[`)
					for i := 0; i < batch; i++ {
						if i > 0 {
							sb.WriteByte(',')
						}
						fmt.Fprintf(&sb, `{"metric":"bench.tp","type":"gauge","interval":0,"tags":["w:%d","s:%d"],"points":[[%d,1]]}`, w, i, base+round)
					}
				} else {
					path = "/v1/logs"
					sb.WriteString(`{"logs":[`)
					for i := 0; i < batch; i++ {
						if i > 0 {
							sb.WriteByte(',')
						}
						fmt.Fprintf(&sb, `{"ts":%d,"message":"bench line %d of round %d","status":"info","service":"bench-%d"}`, (base+round)*1000+int64(i), i, round, w)
					}
				}
				sb.WriteString(`]}`)
				start := time.Now()
				if post(client, ozyd+path, sb.String()) {
					mine = append(mine, time.Since(start))
					requests.Add(1)
					items.Add(int64(batch))
				} else {
					failed.Add(1)
				}
			}
			latMu.Lock()
			lat = append(lat, mine...)
			latMu.Unlock()
		}()
	}
	wg.Wait()
	p50, p95, p99, mx := percentiles(lat)
	secs := d.Seconds()
	fmt.Fprintf(out, "throughput, %s, %d workers, %d per request, %s\n", signal, workers, batch, d)
	fmt.Fprintf(out, "  %.0f requests/s, %.0f %s/s acknowledged (%d requests refused or failed)\n",
		float64(requests.Load())/secs, float64(items.Load())/secs, map[string]string{"series": "points", "logs": "logs"}[signal], failed.Load())
	fmt.Fprintf(out, "  request latency in ms: p50 %.2f  p95 %.2f  p99 %.2f  max %.2f\n", p50, p95, p99, mx)
	return nil
}
