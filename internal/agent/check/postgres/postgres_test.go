package postgres

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

var t0 = time.Unix(1_790_000_000, 0)

// fakeConn answers the check's questions from fields a test sets.
type fakeConn struct {
	conns, maxConns int64
	dbs             []Database
	rels            []Relation
	fail            map[string]error
	gotRelations    []string
	closed          bool
}

func (f *fakeConn) Connections(context.Context) (int64, error) { return f.conns, f.fail["conns"] }
func (f *fakeConn) MaxConnections(context.Context) (int64, error) {
	return f.maxConns, f.fail["max"]
}
func (f *fakeConn) Databases(context.Context) ([]Database, error) { return f.dbs, f.fail["dbs"] }
func (f *fakeConn) Relations(_ context.Context, names []string) ([]Relation, error) {
	f.gotRelations = names
	return f.rels, f.fail["rels"]
}
func (f *fakeConn) Close(context.Context) error { f.closed = true; return nil }

func instance(settings map[string]any) collector.Instance {
	return collector.Instance{Check: "postgres", Name: "postgres", Settings: settings, Clock: testutil.NewFakeClock(t0), Logger: slog.New(slog.DiscardHandler)}
}

func newFake(t *testing.T, settings map[string]any, fc *testutil.FakeClock, cn *fakeConn, dialErr error) *Collector {
	t.Helper()
	inst := instance(settings)
	inst.Clock = fc
	c, err := newCollector(inst, func(context.Context, Config) (conn, error) {
		if dialErr != nil {
			return nil, dialErr
		}
		return cn, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type got map[string][]collector.Metric

func run(c *Collector) (got, error) {
	out := got{}
	err := c.Collect(context.Background(), func(m collector.Metric) { out[m.Name] = append(out[m.Name], m) })
	return out, err
}

func (g got) has(name string, tags ...string) bool {
	for _, m := range g[name] {
		ok := true
		for _, tg := range tags {
			ok = ok && slices.Contains(m.Tags, tg)
		}
		if ok {
			return true
		}
	}
	return false
}

func (g got) one(t *testing.T, name string, tags ...string) float64 {
	t.Helper()
	for _, m := range g[name] {
		ok := true
		for _, tg := range tags {
			ok = ok && slices.Contains(m.Tags, tg)
		}
		if ok {
			return m.Value
		}
	}
	t.Fatalf("no %s%v in %v", name, tags, g[name])
	return 0
}

func TestNew_Settings(t *testing.T) {
	c, err := newCollector(instance(map[string]any{"host": "db", "port": "6543", "user": "u", "password": "p", "relations": []any{"orders"}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.Port != 6543 || c.cfg.DBName != "postgres" || c.cfg.SSLMode != "disable" || c.timeout != 5*time.Second {
		t.Fatalf("cfg %+v timeout %v", c.cfg, c.timeout)
	}
	if !slices.Equal(c.tags, []string{"server:db", "port:6543"}) || c.Name() != "postgres" || c.Interval() != 0 {
		t.Fatalf("tags %v", c.tags)
	}
	if c, err := newCollector(instance(map[string]any{"host": "db", "port": 5433}), nil); err != nil || c.cfg.Port != 5433 {
		t.Fatalf("numeric port: %v", err)
	}
	if _, err := New(instance(map[string]any{"host": "db"})); err != nil {
		t.Fatal(err)
	}
}

func TestNew_Refuses(t *testing.T) {
	many := make([]any, MaxRelations+1)
	for i := range many {
		many[i] = "t"
	}
	for name, tc := range map[string]struct {
		settings map[string]any
		want     string
	}{
		"no host":        {map[string]any{}, "host is required"},
		"port not a num": {map[string]any{"host": "h", "port": "http"}, "port"},
		"port too big":   {map[string]any{"host": "h", "port": 70000}, "port"},
		"bad sslmode":    {map[string]any{"host": "h", "sslmode": "sometimes"}, "sslmode"},
		"bad timeout":    {map[string]any{"host": "h", "timeout": "soon"}, "`soon` into time.Duration"},
		"zero timeout":   {map[string]any{"host": "h", "timeout": "0s"}, "timeout"},
		"too many rels":  {map[string]any{"host": "h", "relations": many}, "at most"},
		"misspelt key":   {map[string]any{"host": "h", "passwrod": "x"}, "passwrod"},
	} {
		_, err := newCollector(instance(tc.settings), nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestCollect_RatesAcrossRuns(t *testing.T) {
	fc := testutil.NewFakeClock(t0)
	cn := &fakeConn{conns: 10, maxConns: 100, dbs: []Database{{Name: "shop", Commits: 100, BlocksHit: 900, BlocksRead: 100, Size: 4096, SizeKnown: true}, {Name: "locked"}}}
	c := newFake(t, map[string]any{"host": "db"}, fc, cn, nil)

	g, err := run(c)
	if err != nil {
		t.Fatal(err)
	}
	if g.one(t, "postgresql.can_connect") != 1 || g.one(t, "postgresql.connections", "server:db", "port:5432") != 10 ||
		g.one(t, "postgresql.max_connections") != 100 || g.one(t, "postgresql.percent_usage_connections") != 10 {
		t.Fatalf("instance metrics %v", g)
	}
	if g.one(t, "postgresql.database_size", "db:shop") != 4096 || len(g["postgresql.database_size"]) != 1 {
		t.Fatalf("size reported for a database without CONNECT: %v", g["postgresql.database_size"])
	}
	if g.has("postgresql.commits", "db:shop") || g.has("postgresql.buffer_hit", "db:shop") {
		t.Fatal("a rate on the first run")
	}
	if !cn.closed {
		t.Error("connection left open")
	}

	fc.Advance(10 * time.Second)
	cn.dbs[0].Commits, cn.dbs[0].BlocksHit, cn.dbs[0].BlocksRead = 150, 1080, 120 // +50 commits; +180 hits, +20 reads
	g, _ = run(c)
	if v := g.one(t, "postgresql.commits", "db:shop"); v != 5 {
		t.Errorf("commits = %v/s, want 5", v)
	}
	if v := g.one(t, "postgresql.buffer_hit", "db:shop"); v != 90 {
		t.Errorf("buffer_hit = %v, want 90 (the interval's, not the lifetime's)", v)
	}
	if g.has("postgresql.buffer_hit", "db:locked") {
		t.Errorf("a hit ratio for a database with no reads: %v", g["postgresql.buffer_hit"])
	}

	// pg_stat_reset(): counters fall; no rate, no spike.
	fc.Advance(10 * time.Second)
	cn.dbs[0].Commits = 3
	g, _ = run(c)
	if g.has("postgresql.commits", "db:shop") {
		t.Errorf("a rate across a reset: %v", g["postgresql.commits"])
	}
}

func TestCollect_CannotConnect(t *testing.T) {
	c := newFake(t, map[string]any{"host": "db"}, testutil.NewFakeClock(t0), nil, errors.New("connection refused"))
	g, err := run(c)
	if err == nil || !strings.Contains(err.Error(), "connection refused") || !strings.Contains(err.Error(), "db:5432") {
		t.Fatalf("err = %v", err)
	}
	if g.one(t, "postgresql.can_connect") != 0 || len(g) != 1 {
		t.Fatalf("emitted %v", g)
	}
}

// A query that fails costs its own metrics, not the others'.
func TestCollect_AFailedQueryKeepsTheRest(t *testing.T) {
	cn := &fakeConn{maxConns: 100, dbs: []Database{{Name: "shop"}}, fail: map[string]error{"conns": errors.New("a"), "rels": errors.New("b")}}
	c := newFake(t, map[string]any{"host": "db", "relations": []any{"orders"}}, testutil.NewFakeClock(t0), cn, nil)
	g, err := run(c)
	if err == nil || !strings.Contains(err.Error(), "connections: a") || !strings.Contains(err.Error(), "relations: b") {
		t.Fatalf("err = %v", err)
	}
	if g.one(t, "postgresql.max_connections") != 100 || len(g["postgresql.connections"]) != 0 || len(g["postgresql.percent_usage_connections"]) != 0 {
		t.Fatalf("emitted %v", g)
	}

	cn.fail = map[string]error{"max": errors.New("c"), "dbs": errors.New("d")}
	_, err = run(c)
	if err == nil || !strings.Contains(err.Error(), "max_connections: c") || !strings.Contains(err.Error(), "databases: d") {
		t.Fatalf("err = %v", err)
	}
}

func TestCollect_Relations(t *testing.T) {
	cn := &fakeConn{rels: []Relation{{Schema: "public", Name: "orders", TableSize: 8192, IndexSize: 2048}}}
	c := newFake(t, map[string]any{"host": "db", "relations": []any{"orders"}}, testutil.NewFakeClock(t0), cn, nil)
	g, err := run(c)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cn.gotRelations, []string{"orders"}) ||
		g.one(t, "postgresql.table_size", "schema:public", "table:orders") != 8192 ||
		g.one(t, "postgresql.index_size", "table:orders") != 2048 {
		t.Fatalf("emitted %v", g)
	}
	// Without relations configured, the query is not run.
	cn.gotRelations = nil
	c = newFake(t, map[string]any{"host": "db"}, testutil.NewFakeClock(t0), cn, nil)
	_, _ = run(c)
	if cn.gotRelations != nil {
		t.Fatal("relations queried without any configured")
	}
}

// The pgx adapter, without a server: nothing listens, so connecting fails
// with pgx's error, and the password is not in it.
func TestDialPgx_Unreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := dialPgx(ctx, Config{Host: "127.0.0.1", Port: 1, User: "u", Password: "s3cr@t/pw", DBName: "postgres", SSLMode: "disable"})
	if err == nil || strings.Contains(err.Error(), "s3cr") {
		t.Fatalf("err = %v", err)
	}
	if _, err := dialPgx(ctx, Config{Host: "127.0.0.1", Port: 1, DBName: "postgres", SSLMode: "bogus"}); err == nil {
		t.Fatal("an unparseable config connected")
	}
}

// Against a real server, when OZY_TEST_POSTGRES_HOST names one
// (OZY_TEST_POSTGRES_PORT, default 5432; OZY_TEST_POSTGRES_USER and
// OZY_TEST_POSTGRES_PASSWORD for the login). CI has no server, so this runs
// by hand; docs/operations.md has the command.
func TestIntegration_RealServer(t *testing.T) {
	host := os.Getenv("OZY_TEST_POSTGRES_HOST")
	if host == "" {
		t.Skip("OZY_TEST_POSTGRES_HOST not set")
	}
	c, err := newCollector(instance(map[string]any{
		"host": host, "port": cmp.Or(os.Getenv("OZY_TEST_POSTGRES_PORT"), "5432"), "user": os.Getenv("OZY_TEST_POSTGRES_USER"), "password": os.Getenv("OZY_TEST_POSTGRES_PASSWORD"),
		"relations": []any{"pg_class"},
	}), dialPgx)
	if err != nil {
		t.Fatal(err)
	}
	c.clock = clock.Real()
	if _, err := run(c); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	g, err := run(c)
	if err != nil {
		t.Fatal(err)
	}
	if g.one(t, "postgresql.can_connect") != 1 || g.one(t, "postgresql.connections") < 1 || len(g["postgresql.commits"]) == 0 ||
		g.one(t, "postgresql.table_size", "schema:pg_catalog", "table:pg_class") <= 0 || g.one(t, "postgresql.database_size", "db:postgres") <= 0 ||
		g.one(t, "postgresql.max_connections") < 1 {
		t.Fatalf("emitted %v", g)
	}
}
