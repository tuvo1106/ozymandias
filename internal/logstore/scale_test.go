package logstore

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// TestScale_OneGiBOfRawLogs is the plan's L12: search latency over about 1 GiB
// of raw logs, label-only against free text, with and without bloom filters.
// It is off unless OZY_SCALE_GIB is set (it writes a few hundred MB and takes
// minutes): OZY_SCALE_GIB=1 go test ./internal/logstore -run Scale -v -timeout 30m
func TestScale_OneGiBOfRawLogs(t *testing.T) {
	gib, _ := strconv.ParseFloat(os.Getenv("OZY_SCALE_GIB"), 64)
	if gib <= 0 {
		t.Skip("set OZY_SCALE_GIB=1 to run")
	}
	target := int64(gib * (1 << 30))
	run := func(t *testing.T, blooms bool) {
		saved := newChunkVersion
		if !blooms {
			newChunkVersion = 1
		}
		defer func() { newChunkVersion = saved }()
		dir := t.TempDir()
		s, _ := openStore(t, dir, func(o *Options) { o.BlockBytes = 256 << 10; o.Retention = -1 })
		defer func() { _ = s.Close() }()
		rng := rand.New(rand.NewPCG(11, 12))
		services := []string{"web-api", "worker", "orders", "billing", "auth", "search", "cart", "mailer"}
		routes := []string{"/api/orders", "/api/users", "/api/items", "/healthz", "/api/search", "/api/cart", "/login", "/logout", "/api/billing"}
		var rare []string
		start := time.Now()
		base := t0.UnixMilli()
		var raw int64
		var n int64
		var batch []wire.Log
		for raw < target {
			svc := services[rng.IntN(len(services))]
			status := "info"
			switch r := rng.IntN(100); {
			case r < 2:
				status = "error"
			case r < 8:
				status = "warn"
			}
			msg := fmt.Sprintf("%s %s %d in %dms for user%d", []string{"GET", "POST", "PUT"}[rng.IntN(3)], routes[rng.IntN(len(routes))], 200+rng.IntN(300), rng.IntN(900), rng.IntN(5000))
			if n%1_000_000 == 777 {
				tok := fmt.Sprintf("incident-%05d", rng.IntN(100000))
				msg += " " + tok
				rare = append(rare, tok)
			}
			l := wire.Log{Ts: base + n*5, Service: svc, Status: status, Host: "box", Source: "json", Message: msg,
				Attrs: map[string]any{"request_id": fmt.Sprintf("req-%08x", rng.Uint32()), "ms": rng.IntN(900), "user": fmt.Sprintf("user%d", rng.IntN(5000))}}
			raw += int64(len(msg)) + 130 // the stored body: message plus ~130 bytes of JSON framing and attributes
			batch = append(batch, l)
			n++
			if len(batch) == 1000 {
				if err := s.Append(context.Background(), batch); err != nil {
					t.Fatal(err)
				}
				batch = batch[:0]
			}
		}
		if err := s.Append(context.Background(), batch); err != nil {
			t.Fatal(err)
		}
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		u := s.Usage()
		t.Logf("blooms=%v: %d logs, %.2f GiB raw -> %.1f MB compressed (%.1fx), filters %.1f MB, ingest %.0f logs/s",
			blooms, u.Entries, float64(u.RawBytes)/(1<<30), float64(u.CompressedBytes)/1e6, float64(u.RawBytes)/float64(u.CompressedBytes),
			float64(u.BloomBytes)/1e6, float64(n)/time.Since(start).Seconds())
		timeIt := func(label, q string) {
			t0 := time.Now()
			res, err := s.Search(context.Background(), mustParse(t, q), wideFrom, int64(1)<<50, SearchOpts{Limit: 100, ScanBudget: 1 << 40})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("  %-34s %8v  logs=%-4d read=%-5d skipped=%-5d streams=%d", label, time.Since(t0).Round(time.Millisecond), len(res.Logs), res.Stats.BlocksRead, res.Stats.BlocksSkipped, res.Stats.Streams)
		}
		timeIt("label only: service:billing status:error", "service:billing status:error")
		timeIt("label + number: @ms:>890", "service:web-api @ms:>890")
		timeIt("free text, common: \"cart\"", "cart")
		if len(rare) > 0 {
			timeIt("free text, rare id", rare[0])
		}
		timeIt("free text, absent", "zzz-not-there-zzz")
		timeIt("attr value, one request id", "@user:user4999 incident")
	}
	t.Run("v1-no-filters", func(t *testing.T) { run(t, false) })
	t.Run("v2-filters", func(t *testing.T) { run(t, true) })
}
