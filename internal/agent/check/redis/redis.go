package redis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/agent/collector/resp"
	"github.com/tuvo1106/ozymandias/internal/clock"
)

// Config is an instance's settings.
type Config struct {
	// Host is the server's name or address, or a unix socket path (starting
	// with "/"). Required.
	Host string `yaml:"host"`
	// Port defaults to 6379. A number or a numeric string, because an
	// autodiscovery label's value is always a string.
	Port Number `yaml:"port"`
	// Username and Password authenticate (Username needs Redis 6 ACLs).
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// DB is selected after connecting. It does not change what INFO reports
	// (INFO is server-wide), but a server whose ACL allows only one database
	// may refuse the connection otherwise.
	DB Number `yaml:"db"`
	// Timeout bounds the connection and INFO. Default 5s.
	Timeout time.Duration `yaml:"timeout"`
}

// Number is an integer setting that also accepts a numeric string.
type Number int

// UnmarshalYAML implements yaml.Unmarshaler.
func (n *Number) UnmarshalYAML(v *yaml.Node) error {
	if v.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: want a number", v.Line)
	}
	i, err := strconv.Atoi(v.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a whole number", v.Line, v.Value)
	}
	*n = Number(i)
	return nil
}

// Check reports one Redis server.
type Check struct {
	cfg   Config
	addr  string
	clock clock.Clock
	rates *collector.Rates
}

var _ collector.Collector = (*Check)(nil)

// New builds a redis check from an instance's settings.
func New(inst collector.Instance) (collector.Collector, error) {
	var cfg Config
	if err := inst.Decode(&cfg); err != nil {
		return nil, err
	}
	if cfg.Host == "" {
		return nil, errors.New("host is required")
	}
	if cfg.Port == 0 {
		cfg.Port = 6379
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("port %d out of range", cfg.Port)
	}
	if cfg.DB < 0 {
		return nil, fmt.Errorf("db %d is negative", cfg.DB)
	}
	if cfg.Timeout < 0 {
		return nil, fmt.Errorf("timeout %v is negative", cfg.Timeout)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	clk := inst.Clock
	if clk == nil {
		clk = clock.Real()
	}
	addr := cfg.Host
	if addr[0] != '/' {
		addr = net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port)))
	}
	return &Check{cfg: cfg, addr: addr, clock: clk, rates: collector.NewRates()}, nil
}

// Name implements collector.Collector; the instance's name replaces it.
func (c *Check) Name() string { return "redis" }

// Interval implements collector.Collector: the scheduler's default.
func (c *Check) Interval() time.Duration { return 0 }

// gauges and rates map INFO fields to metric names. Rates are running
// totals, reported per second.
var (
	gauges = []struct{ field, metric string }{
		{"connected_clients", "redis.net.clients"},
		{"blocked_clients", "redis.net.blocked"},
		{"used_memory", "redis.mem.used"},
		{"used_memory_rss", "redis.mem.rss"},
		{"used_memory_peak", "redis.mem.peak"},
		{"maxmemory", "redis.mem.maxmemory"},
		{"mem_fragmentation_ratio", "redis.mem.fragmentation_ratio"},
		{"uptime_in_seconds", "redis.uptime"},
	}
	rates = []struct{ field, metric string }{
		{"total_commands_processed", "redis.net.commands"},
		{"keyspace_hits", "redis.stats.keyspace_hits"},
		{"keyspace_misses", "redis.stats.keyspace_misses"},
		{"evicted_keys", "redis.keys.evicted"},
		{"expired_keys", "redis.keys.expired"},
		{"rejected_connections", "redis.net.rejected_connections"},
	}
)

// Collect connects, runs INFO, and emits. redis.can_connect is emitted
// whatever happens.
func (c *Check) Collect(ctx context.Context, emit collector.Emit) error {
	now := c.clock.Now()
	defer c.rates.Sweep(now)
	info, err := c.info(ctx)
	if err != nil {
		emit(collector.Metric{Name: "redis.can_connect", Kind: collector.Gauge, Value: 0})
		return err
	}
	emit(collector.Metric{Name: "redis.can_connect", Kind: collector.Gauge, Value: 1})
	for _, g := range gauges {
		if v, ok := info.Number(g.field); ok {
			emit(collector.Metric{Name: g.metric, Kind: collector.Gauge, Value: v})
		}
	}
	for _, r := range rates {
		v, ok := info.Number(r.field)
		if !ok {
			continue
		}
		if rate, ok := c.rates.Observe(r.field, v, now); ok {
			emit(collector.Metric{Name: r.metric, Kind: collector.Rate, Value: rate})
		}
	}
	for _, db := range info.Keyspace() {
		tags := []string{"db:db" + strconv.Itoa(db.DB)}
		emit(collector.Metric{Name: "redis.keys", Kind: collector.Gauge, Value: float64(db.Keys), Tags: tags})
		emit(collector.Metric{Name: "redis.expires", Kind: collector.Gauge, Value: float64(db.Expires), Tags: tags})
	}
	return nil
}

// info runs one connection: dial (with AUTH and SELECT), INFO, close.
// Errors carry the address, never the password.
func (c *Check) info(ctx context.Context) (resp.Info, error) {
	conn, err := resp.Dial(ctx, c.addr, resp.Options{
		Username: c.cfg.Username, Password: c.cfg.Password, DB: int(c.cfg.DB), Timeout: c.cfg.Timeout,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	// Plain INFO: its default sections hold every field used here, and
	// Redis before 7 refuses more than one section argument.
	info, err := conn.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("redis %s: info: %w", c.addr, err)
	}
	return info, nil
}
