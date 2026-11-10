package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/intake"
	"github.com/tuvo1106/ozymandias/internal/logstore"
	"github.com/tuvo1106/ozymandias/internal/query/logql"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// The key property of M7 (spec L2): applying any record sequence with
// arbitrary redelivery leaves each store exactly as applying it once.
//
// Redelivery is modelled as the queue does it. A consumer that crashes before
// committing resumes at its committed offset, so it replays a *suffix of what
// it had applied, in order* — never a record out of the blue and never a
// record before the committed one. After a replay it carries on from where it
// was; so the model applies records in order and, at random points, re-applies
// the last k.
func TestIdempotent_RedeliveryLeavesStoresAsApplyingOnce(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		const base = int64(1790000000)
		nRecords := rapid.IntRange(1, 12).Draw(rt, "records")
		type record struct {
			topic   string
			payload []byte
		}
		var records []record
		next := map[string]int64{} // per-series next timestamp: an agent's points move forward
		for i := 0; i < nRecords; i++ {
			if rapid.Bool().Draw(rt, "isLog") {
				var logs []wire.Log
				for j := range rapid.IntRange(1, 4).Draw(rt, "logs") {
					logs = append(logs, wire.Log{
						Ts: (base + int64(i)) * 1000, Status: "info", Service: "svc",
						Message: fmt.Sprintf("record %d line %d", i, j), // distinct: content dedupe is by design
					})
				}
				b, _ := json.Marshal(logs)
				records = append(records, record{intake.TopicLogs, b})
				continue
			}
			name := rapid.SampledFrom([]string{"m.a", "m.b"}).Draw(rt, "metric")
			var pts []wire.Point
			for range rapid.IntRange(1, 3).Draw(rt, "points") {
				next[name] += 10
				pts = append(pts, wire.Point{Timestamp: base + next[name], Value: float64(rapid.IntRange(0, 9).Draw(rt, "v"))})
			}
			b, _ := json.Marshal([]wire.Series{{Metric: name, Type: wire.KindCount, Interval: 10, Tags: []string{"env:t"}, Points: pts}})
			records = append(records, record{intake.TopicSeries, b})
		}

		run := func(redeliver bool) (series []string, logs []string) {
			cfg := testConfig(t)
			cfg.Storage.MetricStore = "tsdb"
			srv, err := New(cfg, Options{Logger: quiet, Clock: testutil.NewFakeClock(time.Unix(base+1000, 0))})
			if err != nil {
				rt.Fatal(err)
			}
			defer func() { _ = srv.Close() }()
			ctx := context.Background()
			applied := 0
			for i, r := range records {
				if err := srv.intake.Apply(ctx, r.topic, r.payload); err != nil {
					rt.Fatal(err)
				}
				applied++
				if redeliver && rapid.Bool().Draw(rt, fmt.Sprintf("replay%d", i)) {
					k := rapid.IntRange(1, min(applied, 5)).Draw(rt, fmt.Sprintf("k%d", i))
					for _, r := range records[i+1-k : i+1] {
						if err := srv.intake.Apply(ctx, r.topic, r.payload); err != nil {
							rt.Fatal(err)
						}
					}
				}
			}
			for _, m := range []string{"m.a", "m.b"} {
				set, err := srv.store.Select(ctx, tsdb.Selector{Metric: m}, 0, 1<<62)
				if err != nil {
					rt.Fatal(err)
				}
				for set.Next() {
					it := set.Iterator()
					for it.Next() {
						series = append(series, fmt.Sprintf("%s %d %v", set.Series().Key(), it.At().T, it.At().V))
					}
				}
				_ = set.Close()
			}
			q, err := logql.Parse("service:svc")
			if err != nil {
				rt.Fatal(err)
			}
			res, err := srv.logs.Search(ctx, q, 0, 1<<62, logstore.SearchOpts{Limit: 1000})
			if err != nil {
				rt.Fatal(err)
			}
			for _, l := range res.Logs {
				logs = append(logs, fmt.Sprintf("%d %s", l.Ts, l.Message))
			}
			sort.Strings(series)
			sort.Strings(logs)
			return series, logs
		}

		wantS, wantL := run(false)
		gotS, gotL := run(true)
		if fmt.Sprint(wantS) != fmt.Sprint(gotS) {
			rt.Fatalf("series differ after redelivery:\nonce:  %v\ntwice: %v", wantS, gotS)
		}
		if fmt.Sprint(wantL) != fmt.Sprint(gotL) {
			rt.Fatalf("logs differ after redelivery:\nonce:  %v\ntwice: %v", wantL, gotL)
		}
	})
}
