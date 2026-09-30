package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/clock"
)

// MaxRelations bounds the relations setting: each is a series pair, and the
// sizes are computed per run.
const MaxRelations = 100

// Config is one instance's settings.
type Config struct {
	Host     string `yaml:"host"`
	Port     Port   `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
	SSLMode  string `yaml:"sslmode"`
	// Timeout bounds the whole run: connecting and every query.
	Timeout string `yaml:"timeout"`
	// Relations are tables whose table and index sizes are reported. A name
	// matches that table in every schema.
	Relations []string `yaml:"relations"`
}

// Port is a TCP port that accepts a number or a numeric string, because
// settings from container labels (autodiscovery) are always strings.
type Port int

// UnmarshalYAML implements yaml.Unmarshaler.
func (p *Port) UnmarshalYAML(n *yaml.Node) error {
	v, err := strconv.Atoi(n.Value)
	if err != nil || n.Kind != yaml.ScalarNode || v < 1 || v > 65535 {
		return fmt.Errorf("port %q: want a number from 1 to 65535", n.Value)
	}
	*p = Port(v)
	return nil
}

var sslModes = map[string]bool{
	"disable": true, "allow": true, "prefer": true, "require": true, "verify-ca": true, "verify-full": true,
}

// Collector is one postgres instance.
type Collector struct {
	cfg     Config
	timeout time.Duration
	dial    dialer
	clock   clock.Clock
	rates   *collector.Rates
	tags    []string
}

var _ collector.Collector = (*Collector)(nil)

// New is the postgres check's factory.
func New(inst collector.Instance) (collector.Collector, error) {
	return newCollector(inst, dialPgx)
}

func newCollector(inst collector.Instance, dial dialer) (*Collector, error) {
	cfg := Config{Port: 5432, DBName: "postgres", SSLMode: "disable", Timeout: "5s"}
	if err := inst.Decode(&cfg); err != nil {
		return nil, err
	}
	if cfg.Host == "" {
		return nil, errors.New("host is required")
	}
	if !sslModes[cfg.SSLMode] {
		return nil, fmt.Errorf("sslmode %q: want disable, allow, prefer, require, verify-ca or verify-full", cfg.SSLMode)
	}
	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil || timeout <= 0 {
		return nil, fmt.Errorf("timeout %q: want a positive duration, like 5s", cfg.Timeout)
	}
	if len(cfg.Relations) > MaxRelations {
		return nil, fmt.Errorf("relations: %d listed, at most %d", len(cfg.Relations), MaxRelations)
	}
	clk := inst.Clock
	if clk == nil {
		clk = clock.Real()
	}
	return &Collector{
		cfg: cfg, timeout: timeout, dial: dial, clock: clk, rates: collector.NewRates(),
		tags: []string{"server:" + cfg.Host, "port:" + strconv.Itoa(int(cfg.Port))},
	}, nil
}

// Name implements collector.Collector; the instance wrapper names it.
func (c *Collector) Name() string { return "postgres" }

// Interval implements collector.Collector: the scheduler's default.
func (c *Collector) Interval() time.Duration { return 0 }

// dialer opens a connection; the pgx one in production, a fake in tests.
type dialer func(ctx context.Context, cfg Config) (conn, error)

// conn is the SQL the check runs, one method per question. Keeping it this
// narrow is what lets every other line be tested without a server.
type conn interface {
	Connections(ctx context.Context) (int64, error)
	MaxConnections(ctx context.Context) (int64, error)
	Databases(ctx context.Context) ([]Database, error)
	Relations(ctx context.Context, names []string) ([]Relation, error)
	Close(ctx context.Context) error
}

// Database is one row of pg_stat_database, template databases excluded.
// The counters are cumulative since the statistics were last reset.
type Database struct {
	Name                        string
	Commits, Rollbacks          int64
	Returned, Fetched, Inserted int64
	Updated, Deleted            int64
	Deadlocks, TempBytes        int64
	BlocksHit, BlocksRead       int64
	Size                        int64
	SizeKnown                   bool // false without CONNECT privilege
}

// Relation is one table's sizes.
type Relation struct {
	Schema, Name         string
	TableSize, IndexSize int64
}

// Collect connects, reads, and disconnects. postgresql.can_connect is
// always emitted — 0 with the error when the server cannot be reached — so a
// dashboard can tell "down" from "not configured".
func (c *Collector) Collect(ctx context.Context, emit collector.Emit) error {
	now := c.clock.Now()
	defer c.rates.Sweep(now)
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	gauge := func(name string, v float64, extra ...string) {
		emit(collector.Metric{Name: name, Kind: collector.Gauge, Value: v, Tags: append(append([]string(nil), c.tags...), extra...)})
	}

	cn, err := c.dial(ctx, c.cfg)
	if err != nil {
		gauge("postgresql.can_connect", 0)
		return fmt.Errorf("connecting to %s:%d: %w", c.cfg.Host, c.cfg.Port, err)
	}
	defer func() { _ = cn.Close(context.WithoutCancel(ctx)) }()
	gauge("postgresql.can_connect", 1)

	var errs []error
	n, err := cn.Connections(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("connections: %w", err))
	} else {
		gauge("postgresql.connections", float64(n))
	}
	maxConn, err := cn.MaxConnections(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("max_connections: %w", err))
	} else {
		gauge("postgresql.max_connections", float64(maxConn))
		if n > 0 && maxConn > 0 {
			gauge("postgresql.percent_usage_connections", 100*float64(n)/float64(maxConn))
		}
	}

	dbs, err := cn.Databases(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("databases: %w", err))
	}
	for _, db := range dbs {
		c.database(db, now, gauge)
	}

	if len(c.cfg.Relations) > 0 {
		rels, err := cn.Relations(ctx, c.cfg.Relations)
		if err != nil {
			errs = append(errs, fmt.Errorf("relations: %w", err))
		}
		for _, r := range rels {
			gauge("postgresql.table_size", float64(r.TableSize), "schema:"+r.Schema, "table:"+r.Name)
			gauge("postgresql.index_size", float64(r.IndexSize), "schema:"+r.Schema, "table:"+r.Name)
		}
	}
	return errors.Join(errs...)
}

// database emits one database's metrics. The activity counters become
// per-second rates; the hit ratio is computed from the two block rates,
// which over one interval is the ratio of the blocks read in it — not the
// lifetime ratio pg_stat_database would give, which a week-old server
// pins near its long-run average however bad the last minute was.
func (c *Collector) database(db Database, now time.Time, gauge func(string, float64, ...string)) {
	tag := "db:" + db.Name
	rate := func(name string, v int64) (float64, bool) {
		r, ok := c.rates.Observe(db.Name+"\x00"+name, float64(v), now)
		if ok {
			gauge(name, r, tag)
		}
		return r, ok
	}
	rate("postgresql.commits", db.Commits)
	rate("postgresql.rollbacks", db.Rollbacks)
	rate("postgresql.rows_returned", db.Returned)
	rate("postgresql.rows_fetched", db.Fetched)
	rate("postgresql.rows_inserted", db.Inserted)
	rate("postgresql.rows_updated", db.Updated)
	rate("postgresql.rows_deleted", db.Deleted)
	rate("postgresql.deadlocks", db.Deadlocks)
	rate("postgresql.temp_bytes", db.TempBytes)
	hit, okHit := c.rates.Observe(db.Name+"\x00hit", float64(db.BlocksHit), now)
	read, okRead := c.rates.Observe(db.Name+"\x00read", float64(db.BlocksRead), now)
	if okHit && okRead && hit+read > 0 {
		gauge("postgresql.buffer_hit", 100*hit/(hit+read), tag)
	}
	if db.SizeKnown {
		gauge("postgresql.database_size", float64(db.Size), tag)
	}
}
