package logstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/query/logql"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// tb is what the helpers need of a test, so they work under *testing.T and *rapid.T.
type tb interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
}

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// openStore opens a store in dir with a fake clock and small blocks, so a few
// dozen logs already make several blocks.
func openStore(t tb, dir string, tune ...func(*Options)) (*Store, *testutil.FakeClock) {
	t.Helper()
	return openStoreAt(t, dir, t0, tune...)
}

// openStoreAt is openStore with the fake clock starting at start.
func openStoreAt(t tb, dir string, start time.Time, tune ...func(*Options)) (*Store, *testutil.FakeClock) {
	t.Helper()
	clk := testutil.NewFakeClock(start)
	o := Options{Dir: dir, Clock: clk, BlockBytes: 2 << 10, NoSync: true, Logger: quietLogger(), Tick: time.Second}
	for _, f := range tune {
		f(&o)
	}
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	return s, clk
}

func mkLog(ts int64, svc, status, msg string, ms int) wire.Log {
	return wire.Log{
		Ts: ts, Message: msg, Status: status, Service: svc, Source: "src", Host: "h1",
		Tags:  []string{"env:dev"},
		Attrs: map[string]any{"ms": json.Number(fmt.Sprint(ms))},
	}
}

func mustParse(t tb, q string) logql.Node {
	t.Helper()
	n, err := logql.Parse(q)
	if err != nil {
		t.Fatalf("Parse(%q): %v", q, err)
	}
	return n
}

// all pages through a search and returns every log, failing if a page repeats
// or the paging does not end.
func all(t tb, s *Store, q string, from, to int64, opts SearchOpts) []wire.Log {
	t.Helper()
	var out []wire.Log
	for pages := 0; ; pages++ {
		if pages > 100000 {
			t.Fatal("paging does not end")
		}
		res, err := s.Search(context.Background(), mustParse(t, q), from, to, opts)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, res.Logs...)
		if res.Cursor == "" {
			return out
		}
		opts.Cursor = res.Cursor
	}
}

func jsonOf(l wire.Log) string { b, _ := json.Marshal(l); return string(b) }

func jsons(ls []wire.Log) []string {
	out := make([]string, len(ls))
	for i := range ls {
		out[i] = jsonOf(ls[i])
	}
	return out
}

const (
	wideFrom = int64(0)
	wideTo   = int64(1) << 60
)

func TestStore_AppendThenSearchBeforeAndAfterFlush(t *testing.T) {
	s, _ := openStore(t, t.TempDir())
	defer func() { _ = s.Close() }()
	base := t0.UnixMilli()
	var batch []wire.Log
	for i := 0; i < 50; i++ {
		batch = append(batch, mkLog(base+int64(i)*1000, "api", "info", fmt.Sprintf("request %d", i), i))
	}
	if err := s.Append(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	head := all(t, s, "service:api", wideFrom, wideTo, SearchOpts{})
	if len(head) != 50 || head[0].Message != "request 49" || head[49].Message != "request 0" {
		t.Fatalf("newest-first over the head: %d logs, first %q last %q", len(head), head[0].Message, head[len(head)-1].Message)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	sealed := all(t, s, "service:api", wideFrom, wideTo, SearchOpts{})
	if strings.Join(jsons(head), "\n") != strings.Join(jsons(sealed), "\n") {
		t.Fatal("sealing a head changed what a search returns")
	}
	oldest := all(t, s, "service:api", wideFrom, wideTo, SearchOpts{Order: Oldest})
	if oldest[0].Message != "request 0" || len(oldest) != 50 {
		t.Fatalf("oldest-first: first %q, %d logs", oldest[0].Message, len(oldest))
	}
}

func TestStore_RangeIsInclusiveOnBothEnds(t *testing.T) {
	s, _ := openStore(t, t.TempDir())
	defer func() { _ = s.Close() }()
	base := t0.UnixMilli()
	var batch []wire.Log
	for i := 0; i < 10; i++ {
		batch = append(batch, mkLog(base+int64(i), "api", "info", fmt.Sprint(i), 0))
	}
	_ = s.Append(context.Background(), batch)
	for _, flushed := range []bool{false, true} {
		if flushed {
			_ = s.Flush()
		}
		got := all(t, s, "", base+3, base+6, SearchOpts{Order: Oldest})
		if len(got) != 4 || got[0].Message != "3" || got[3].Message != "6" {
			t.Errorf("flushed=%v: [3,6] gave %v", flushed, jsons(got))
		}
	}
}

func TestStore_PagingNeverRepeatsOrSkips(t *testing.T) {
	s, _ := openStore(t, t.TempDir())
	defer func() { _ = s.Close() }()
	base := t0.UnixMilli()
	var batch []wire.Log
	for i := 0; i < 200; i++ { // many share a timestamp: the cursor must break ties
		batch = append(batch, mkLog(base+int64(i/7), "api", "info", fmt.Sprint(i), 0))
	}
	_ = s.Append(context.Background(), batch[:120])
	_ = s.Flush()
	_ = s.Append(context.Background(), batch[120:])
	want := all(t, s, "", wideFrom, wideTo, SearchOpts{Limit: 1000})
	for _, order := range []Order{Newest, Oldest} {
		for _, limit := range []int{1, 3, 7, 50, 199, 200, 201} {
			got := all(t, s, "", wideFrom, wideTo, SearchOpts{Limit: limit, Order: order})
			ref := want
			if order == Oldest {
				ref = all(t, s, "", wideFrom, wideTo, SearchOpts{Limit: 1000, Order: Oldest})
			}
			if strings.Join(jsons(got), "\n") != strings.Join(jsons(ref), "\n") {
				t.Fatalf("order %v limit %d: paging gave %d logs that differ from one big page (%d)", order, limit, len(got), len(ref))
			}
		}
	}
}

// Logs arriving between pages must not make a page repeat or drop a log that
// was there when paging began.
func TestStore_PagingStaysStableWhileIngestContinues(t *testing.T) {
	s, _ := openStore(t, t.TempDir())
	defer func() { _ = s.Close() }()
	base := t0.UnixMilli()
	var first []wire.Log
	for i := 0; i < 100; i++ {
		first = append(first, mkLog(base+int64(i)*10, "api", "info", fmt.Sprintf("orig %d", i), 0))
	}
	_ = s.Append(context.Background(), first)
	seen := map[string]int{}
	opts := SearchOpts{Limit: 10}
	for page := 0; ; page++ {
		res, err := s.Search(context.Background(), mustParse(t, ""), wideFrom, wideTo, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range res.Logs {
			seen[l.Message]++
		}
		// Between pages: newer logs, older logs, a flush.
		_ = s.Append(context.Background(), []wire.Log{
			mkLog(base+100000+int64(page), "api", "info", fmt.Sprintf("newer %d", page), 0),
			mkLog(base-1000-int64(page), "api", "info", fmt.Sprintf("older %d", page), 0),
		})
		if page%3 == 0 {
			_ = s.Flush()
		}
		if res.Cursor == "" {
			break
		}
		opts.Cursor = res.Cursor
	}
	for i := 0; i < 100; i++ {
		if n := seen[fmt.Sprintf("orig %d", i)]; n != 1 {
			t.Errorf("orig %d seen %d times", i, n)
		}
	}
	for m, n := range seen {
		if n != 1 {
			t.Errorf("%q seen %d times", m, n)
		}
		if strings.HasPrefix(m, "newer") {
			t.Errorf("%q arrived after the first page and sorts before the cursor, but appeared", m)
		}
	}
}

// --- the differential property -------------------------------------------

var (
	pSvc    = []string{"api", "worker", "web"}
	pStatus = []string{"debug", "info", "warn", "error"}
	pHost   = []string{"", "h1", "h2"}
	pEnv    = []string{"", "dev", "prod"}
	pWords  = []string{"timeout", "refused", "ok", "slow", "retry", "Épée", "zebra", "quokka", "Alpha-Beta", "gamma_delta"}
	pUsers  = []string{"alice", "bob", "carol", "dave", "erin", "Zoë"}
)

func randLog(t *rapid.T, base int64, label string) wire.Log {
	l := wire.Log{
		Ts:      base + rapid.Int64Range(-2*86_400_000, 2*86_400_000).Draw(t, label+"ts"),
		Service: rapid.SampledFrom(pSvc).Draw(t, label+"svc"),
		Status:  rapid.SampledFrom(pStatus).Draw(t, label+"st"),
		Host:    rapid.SampledFrom(pHost).Draw(t, label+"host"),
		Message: rapid.SampledFrom(pWords).Draw(t, label+"w1") + " " + rapid.SampledFrom(pWords).Draw(t, label+"w2"),
		Attrs: map[string]any{
			"ms":   json.Number(fmt.Sprint(rapid.IntRange(0, 4).Draw(t, label+"ms") * 100)),
			"code": json.Number(fmt.Sprint(rapid.SampledFrom([]int{200, 404, 500}).Draw(t, label+"code"))),
			// Words that are rare per block, so a bloom filter has something to rule out.
			"user":  rapid.SampledFrom(pUsers).Draw(t, label+"user"),
			"flags": []any{rapid.SampledFrom(pUsers).Draw(t, label+"f1"), true},
		},
	}
	if env := rapid.SampledFrom(pEnv).Draw(t, label+"env"); env != "" {
		l.Tags = []string{"env:" + env}
	}
	return l
}

var pTerms = []string{
	"service:api", "service:w*", "status:error", "status:warn", "host:h1", "host:*", "env:dev", "env:prod",
	"timeout", "refused", "tim*out", "slow retry", "épée", "@ms:>200", "@ms:<=100", "@code:500", "@code:[200 TO 404]",
	"-status:debug", "-timeout", "-host:*", "-@code:200",
	"zebra", "quok*", "-quokka", "alice", "ALICE", "@user:bob", "@user:car*", "@user:zoë", "alpha-beta", "gamma_d*a", "ali",
	"@flags:erin", "@flags:true", "zebra OR quokka", "(alice OR dave) timeout", "z*a", "@user:*",
}

func randQuery(t *rapid.T, label string) string {
	n := rapid.IntRange(0, 3).Draw(t, label+"n")
	var parts []string
	for i := 0; i < n; i++ {
		parts = append(parts, rapid.SampledFrom(pTerms).Draw(t, fmt.Sprintf("%sterm%d", label, i)))
	}
	q := strings.Join(parts, " ")
	if n >= 2 && rapid.Bool().Draw(t, label+"or") {
		q = strings.Join(parts[:1], " ") + " OR " + strings.Join(parts[1:], " ")
	}
	if n >= 3 && rapid.Bool().Draw(t, label+"paren") {
		q = "(" + parts[0] + " OR " + parts[1] + ") " + parts[2]
	}
	return q
}

type oracleLog struct {
	l   wire.Log
	idx int
}

// expected is the brute-force answer: every log ever appended that satisfies
// the whole query, in the store's order. It uses logql.Compile on the whole
// query and nothing of the index, the planner or the blocks.
func expected(t tb, logs []oracleLog, q string, from, to int64, order Order) []string {
	t.Helper()
	f := logql.Compile(mustParse(t, q))
	var m []oracleLog
	for _, ol := range logs {
		if ol.l.Ts >= from && ol.l.Ts <= to && f(&ol.l) {
			m = append(m, ol)
		}
	}
	sort.SliceStable(m, func(i, j int) bool {
		if m[i].l.Ts != m[j].l.Ts {
			return (m[i].l.Ts < m[j].l.Ts) == (order == Oldest)
		}
		return (m[i].idx < m[j].idx) == (order == Oldest)
	})
	out := make([]string, len(m))
	for i := range m {
		out[i] = jsonOf(m[i].l)
	}
	return out
}

func TestProperty_SearchEqualsBruteForce(t *testing.T) {
	skipped, read := 0, 0
	t.Cleanup(func() {
		// Agreeing while never skipping would prove nothing about the filters.
		if !t.Failed() && skipped == 0 {
			t.Errorf("no block was ever ruled out by a bloom filter (%d read): the generators never exercise them", read)
		}
	})
	rapid.Check(t, func(t *rapid.T) {
		dir, err := os.MkdirTemp("", "logstore-prop")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		s, _ := openStore(t, dir)
		defer func() { _ = s.Close() }()
		base := t0.UnixMilli()

		var oracle []oracleLog
		ops := rapid.IntRange(1, 12).Draw(t, "ops")
		for o := 0; o < ops; o++ {
			switch rapid.IntRange(0, 5).Draw(t, fmt.Sprintf("op%d", o)) {
			case 0:
				if err := s.Flush(); err != nil {
					t.Fatal(err)
				}
			case 1: // a clean restart: heads are sealed on close, footers written
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, _ = openStore(t, dir)
			default:
				n := rapid.IntRange(1, 25).Draw(t, fmt.Sprintf("n%d", o))
				var batch []wire.Log
				for i := 0; i < n; i++ {
					l := randLog(t, base, fmt.Sprintf("b%d.%d", o, i))
					batch = append(batch, l)
					oracle = append(oracle, oracleLog{l: l, idx: len(oracle)})
				}
				if err := s.Append(context.Background(), batch); err != nil {
					t.Fatal(err)
				}
			}
		}

		for qi := 0; qi < 6; qi++ {
			q := randQuery(t, fmt.Sprintf("q%d", qi))
			order := Order(rapid.IntRange(0, 1).Draw(t, fmt.Sprintf("order%d", qi)))
			from := base + rapid.Int64Range(-3*86_400_000, 0).Draw(t, fmt.Sprintf("from%d", qi))
			to := base + rapid.Int64Range(0, 3*86_400_000).Draw(t, fmt.Sprintf("to%d", qi))
			limit := rapid.SampledFrom([]int{1, 5, 17, 1000}).Draw(t, fmt.Sprintf("limit%d", qi))
			got := jsons(all(t, s, q, from, to, SearchOpts{Order: order, Limit: limit}))
			full, err := s.Search(context.Background(), mustParse(t, q), from, to, SearchOpts{Limit: 1000})
			if err != nil {
				t.Fatal(err)
			}
			skipped += full.Stats.BlocksSkipped
			read += full.Stats.BlocksRead
			want := expected(t, oracle, q, from, to, order)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("query %q [%d,%d] order %v limit %d: store returned %d logs, brute force %d\nfirst difference: %s",
					q, from, to, order, limit, len(got), len(want), firstDiff(got, want))
			}
		}
	})
}

func firstDiff(a, b []string) string {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return fmt.Sprintf("at %d: store %s vs brute force %s", i, a[i], b[i])
		}
	}
	return fmt.Sprintf("lengths %d vs %d", len(a), len(b))
}

func TestStore_UsageCountsWhatIsHeldAndWhatIsSealed(t *testing.T) {
	s, _ := openStore(t, t.TempDir())
	defer func() { _ = s.Close() }()
	if u := s.Usage(); u != (Usage{}) {
		t.Fatalf("an empty store reports %+v", u)
	}
	logs := []wire.Log{
		{Ts: t0.UnixMilli(), Message: "one", Status: "info", Service: "a"},
		{Ts: t0.UnixMilli() + 1, Message: "two", Status: "error", Service: "a"},
		{Ts: t0.UnixMilli() + 2, Message: "three", Status: "info", Service: "b"},
	}
	if err := s.Append(context.Background(), logs); err != nil {
		t.Fatal(err)
	}
	u := s.Usage()
	if u.Streams != 3 || u.HeadEntries != 3 || u.HeadBytes <= 0 || u.Entries != 0 || u.Blocks != 0 {
		t.Fatalf("before a seal: %+v", u)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	u = s.Usage()
	if u.Entries != 3 || u.HeadEntries != 0 || u.HeadBytes != 0 || u.Chunks != 3 || u.Blocks != 3 {
		t.Fatalf("after a seal: %+v", u)
	}
	if u.RawBytes <= 0 || u.CompressedBytes <= 0 || u.BloomBytes <= 0 {
		t.Fatalf("sizes: %+v", u)
	}
}
