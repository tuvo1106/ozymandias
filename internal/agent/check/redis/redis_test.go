package redis

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

var t0 = time.Unix(1_790_000_000, 0)

// server is a fake Redis: it answers AUTH (against password, if set),
// SELECT, and INFO with the fixture, filled with the current totals.
type server struct {
	ln       net.Listener
	password string

	mu       sync.Mutex
	commands int
	hits     int
	seen     [][]string
}

func newServer(t *testing.T, password string) *server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{ln: ln, password: password, commands: 1000, hits: 10}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *server) port() string { return strconv.Itoa(s.ln.Addr().(*net.TCPAddr).Port) }

func (s *server) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *server) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	tmpl, _ := os.ReadFile("testdata/info.txt.tmpl")
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.seen = append(s.seen, args)
		body := fmt.Sprintf(string(tmpl), strconv.Itoa(s.commands), strconv.Itoa(s.hits))
		s.mu.Unlock()
		switch strings.ToUpper(args[0]) {
		case "AUTH":
			if args[len(args)-1] != s.password {
				_, _ = c.Write([]byte("-WRONGPASS invalid username-password pair or user is disabled.\r\n"))
				continue
			}
			_, _ = c.Write([]byte("+OK\r\n"))
		case "SELECT":
			_, _ = c.Write([]byte("+OK\r\n"))
		case "INFO":
			_, _ = fmt.Fprintf(c, "$%d\r\n%s\r\n", len(body), body)
		default:
			_, _ = c.Write([]byte("-ERR unknown command\r\n"))
		}
	}
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := range args {
		if _, err := r.ReadString('\n'); err != nil { // $len
			return nil, err
		}
		v, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		args[i] = strings.TrimSuffix(v, "\r\n")
	}
	return args, nil
}

func (s *server) advance(commands, hits int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands += commands
	s.hits += hits
}

func newCheck(t *testing.T, settings map[string]any, fc *testutil.FakeClock) *Check {
	t.Helper()
	c, err := New(collector.Instance{Check: "redis", Name: "redis", Settings: settings, Clock: fc})
	if err != nil {
		t.Fatal(err)
	}
	return c.(*Check)
}

type got map[string][]collector.Metric

func run(c *Check) (got, error) {
	out := got{}
	err := c.Collect(context.Background(), func(m collector.Metric) { out[m.Name] = append(out[m.Name], m) })
	return out, err
}

func (g got) value(t *testing.T, name string) float64 {
	t.Helper()
	ms := g[name]
	if len(ms) != 1 {
		t.Fatalf("%s: %d points, want 1 (%v)", name, len(ms), ms)
	}
	return ms[0].Value
}

func TestRedis_GaugesAndRates(t *testing.T) {
	s := newServer(t, "")
	fc := testutil.NewFakeClock(t0)
	c := newCheck(t, map[string]any{"host": "127.0.0.1", "port": s.port()}, fc) // port as a string, as from a label

	first, err := run(c)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]float64{
		"redis.can_connect": 1, "redis.net.clients": 3, "redis.net.blocked": 1,
		"redis.mem.used": 2097152, "redis.mem.rss": 8388608, "redis.mem.peak": 4194304,
		"redis.mem.maxmemory": 104857600, "redis.mem.fragmentation_ratio": 3.95, "redis.uptime": 3600,
	} {
		if v := first.value(t, name); v != want {
			t.Errorf("%s = %v, want %v", name, v, want)
		}
	}
	if len(first["redis.net.commands"]) != 0 {
		t.Error("a rate on the first run: there is nothing to subtract from")
	}
	keys := map[string]float64{}
	for _, m := range first["redis.keys"] {
		keys[strings.Join(m.Tags, ",")] = m.Value
	}
	if keys["db:db0"] != 42 || keys["db:db3"] != 7 || len(keys) != 2 {
		t.Errorf("redis.keys = %v", keys)
	}

	s.advance(1500, 30)
	fc.Advance(15 * time.Second)
	second, err := run(c)
	if err != nil {
		t.Fatal(err)
	}
	if v := second.value(t, "redis.net.commands"); v != 100 {
		t.Errorf("commands/s = %v, want 100", v)
	}
	if v := second.value(t, "redis.stats.keyspace_hits"); v != 2 {
		t.Errorf("hits/s = %v, want 2", v)
	}
	if v := second.value(t, "redis.stats.keyspace_misses"); v != 0 {
		t.Errorf("misses/s = %v, want 0", v)
	}
	for _, name := range []string{"redis.keys.evicted", "redis.keys.expired", "redis.net.rejected_connections"} {
		if m := second[name]; len(m) != 1 || m[0].Kind != collector.Rate {
			t.Errorf("%s = %v, want one rate", name, m)
		}
	}

	// A restart: the totals go down, so no rate, and no negative spike.
	s.advance(-2000, 0)
	fc.Advance(15 * time.Second)
	third, _ := run(c)
	if len(third["redis.net.commands"]) != 0 {
		t.Errorf("a rate across a restart: %v", third["redis.net.commands"])
	}
}

func TestRedis_AuthAndSelect(t *testing.T) {
	s := newServer(t, "s3cret")
	c := newCheck(t, map[string]any{"host": "127.0.0.1", "port": s.port(), "password": "s3cret", "db": "2"}, testutil.NewFakeClock(t0))
	if _, err := run(c); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen) < 3 || s.seen[0][0] != "AUTH" || s.seen[1][0] != "SELECT" || s.seen[1][1] != "2" || s.seen[2][0] != "INFO" || len(s.seen[2]) != 1 {
		t.Fatalf("commands %v", s.seen)
	}
}

func TestRedis_WrongPassword(t *testing.T) {
	s := newServer(t, "right")
	c := newCheck(t, map[string]any{"host": "127.0.0.1", "port": s.port(), "password": "wrong-horse"}, testutil.NewFakeClock(t0))
	g, err := run(c)
	if err == nil || !strings.Contains(err.Error(), "WRONGPASS") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "wrong-horse") {
		t.Fatal("the password is in the error")
	}
	if g.value(t, "redis.can_connect") != 0 || len(g) != 1 {
		t.Fatalf("emitted %v", g)
	}
}

func TestRedis_Unreachable(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	c := newCheck(t, map[string]any{"host": "127.0.0.1", "port": port, "timeout": "1s"}, testutil.NewFakeClock(t0))
	g, err := run(c)
	if err == nil || g.value(t, "redis.can_connect") != 0 {
		t.Fatalf("err %v, emitted %v", err, g)
	}
}

// INFO answered with something other than text breaks the run, and says so.
func TestRedis_BadInfoReply(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = readCommand(bufio.NewReader(c))
		_, _ = c.Write([]byte(":7\r\n"))
	}()
	c := newCheck(t, map[string]any{"host": "127.0.0.1", "port": ln.Addr().(*net.TCPAddr).Port}, testutil.NewFakeClock(t0))
	g, err := run(c)
	if err == nil || !strings.Contains(err.Error(), "info") || g.value(t, "redis.can_connect") != 0 {
		t.Fatalf("err %v, emitted %v", err, g)
	}
}

func TestNew_Settings(t *testing.T) {
	c := newCheck(t, map[string]any{"host": "cache"}, nil)
	if c.addr != "cache:6379" || c.cfg.Timeout != 5*time.Second || c.Name() != "redis" || c.Interval() != 0 {
		t.Fatalf("defaults: %+v %s", c.cfg, c.addr)
	}
	if c := newCheck(t, map[string]any{"host": "/run/redis.sock"}, nil); c.addr != "/run/redis.sock" {
		t.Errorf("unix socket addr %q", c.addr)
	}
	if c := newCheck(t, map[string]any{"host": "::1", "port": 7000}, nil); c.addr != "[::1]:7000" {
		t.Errorf("ipv6 addr %q", c.addr)
	}
	for name, settings := range map[string]map[string]any{
		"no host":           {},
		"misspelt setting":  {"host": "x", "passwrod": "p"},
		"port not a number": {"host": "x", "port": "six"},
		"port a list":       {"host": "x", "port": []any{1}},
		"port too big":      {"host": "x", "port": 70000},
		"negative db":       {"host": "x", "db": -1},
		"negative timeout":  {"host": "x", "timeout": "-1s"},
	} {
		if _, err := New(collector.Instance{Name: "redis", Settings: settings}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Through the registry, the way the agent builds it.
func TestNew_ThroughTheRegistry(t *testing.T) {
	reg := collector.Registry{"redis": New}
	c, err := reg.NewInstance("redis", 0, 1, map[string]any{"name": "cache", "host": "x", "tags": []any{"team:web"}}, nil, nil, nil)
	if err != nil || c.Name() != "redis:cache" {
		t.Fatalf("%v, %v", c, err)
	}
	if _, err := reg.NewInstance("redis", 0, 1, map[string]any{"host": "x", "bogus": 1}, nil, nil, nil); err == nil || !errors.Is(err, err) || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("unknown setting: %v", err)
	}
}
