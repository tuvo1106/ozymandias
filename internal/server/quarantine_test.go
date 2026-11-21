package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/block"
	"github.com/tuvo1106/ozymandias/internal/tsdb/db"
)

type safeBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var _ io.Writer = (*safeBuf)(nil)

// pointsOf runs a query through the HTTP API and returns the start of every bucket that holds a value.
func pointsOf(t *testing.T, h http.Handler, fromS, toS int64) []int64 {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/query?metric=m&agg=sum&interval=600&from="+itoa(fromS)+"&to="+itoa(toS), nil))
	if w.Code != 200 {
		t.Fatalf("query: %d %s", w.Code, w.Body.String())
	}
	var res struct {
		Series []struct {
			Points [][]*float64 `json:"points"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, s := range res.Series {
		for _, p := range s.Points {
			if len(p) == 2 && p[1] != nil {
				out = append(out, int64(*p[0])/1000) // the API's times are milliseconds
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// The plan's row for block corruption: a damaged block is detected, set aside, the rest is served, and the
// whole thing is loud (a log line and a self-metric). Through the real server on a real data directory.
func TestServer_StartsWithADamagedBlockAndSaysSo(t *testing.T) {
	cfg := testConfig(t)
	tsdbPath := filepath.Join(cfg.DataDir, tsdbDir)
	store, err := db.Open(db.Options{Dir: tsdbPath, BlockRange: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// Ten-minute samples from 00:00 to 05:00 (1970): two blocks, [0 h, 2 h) and [2 h, 4 h), and 4 h..5 h left in the log.
	var batch tsdb.SeriesSamples
	batch.Series = tsdb.NewSeriesRef("m", nil)
	for s := int64(0); s <= 5*3600; s += 600 {
		batch.Samples = append(batch.Samples, tsdb.Sample{T: s * 1000, V: 1})
	}
	if _, err := store.Append(context.Background(), []tsdb.SeriesSamples{batch}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := store.CutBlock(); err != nil {
			t.Fatalf("cut %d: %v", i, err)
		}
	}
	var dirs []string
	for _, b := range store.Blocks() {
		dirs = append(dirs, b.Dir())
	}
	sort.Strings(dirs)
	if len(dirs) != 2 {
		t.Fatalf("%d blocks, want 2", len(dirs))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Damage the first block (0 h to 2 h): flip a byte of its index.
	idx := filepath.Join(dirs[0], block.IndexFilename)
	b, err := os.ReadFile(idx)
	if err != nil {
		t.Fatal(err)
	}
	b[40] ^= 0xFF
	if err := os.WriteFile(idx, b, 0o644); err != nil {
		t.Fatal(err)
	}

	journal := &safeBuf{}
	s, err := New(cfg, Options{Logger: slog.New(slog.NewTextHandler(journal, nil))})
	if err != nil {
		t.Fatalf("the server did not start with one damaged block: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Ready, and says so: a damaged block is data missing, not a store that has stopped.
	if code, body := readyz(s.Handler()); code != 200 || body["status"] != "ready" {
		t.Errorf("/readyz = %d %v, want 200 ready", code, body)
	}
	// Loud: the self-metric counts it from the first scrape.
	var gauge, reads = -1.0, -1.0
	for _, p := range s.reg.Snapshot() {
		switch p.Name {
		case "ozy.tsdb.blocks_quarantined":
			gauge = p.Value
		case "ozy.tsdb.corrupt_reads":
			reads = p.Value
		}
	}
	if gauge != 1 || reads != 0 {
		t.Errorf("ozy.tsdb.blocks_quarantined = %v and ozy.tsdb.corrupt_reads = %v, want 1 and 0", gauge, reads)
	}
	if log := journal.String(); !strings.Contains(log, "block quarantined") || !strings.Contains(log, "level=ERROR") {
		t.Errorf("no error line says a block was quarantined:\n%s", log)
	}
	// The rest is served: every ten-minute bucket from 2 h on, none of the damaged block's two hours.
	var want []int64
	for s := int64(7200); s <= 5*3600; s += 600 {
		want = append(want, s)
	}
	if got := pointsOf(t, s.Handler(), 0, 5*3600+600); !slices.Equal(got, want) {
		t.Errorf("the API served %d buckets %v, want the %d from 2 h on: %v", len(got), got, len(want), want)
	}
}
