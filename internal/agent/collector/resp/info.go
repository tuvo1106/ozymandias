package resp

import (
	"sort"
	"strconv"
	"strings"
)

// Info is a parsed INFO reply: section name (lower case, e.g. "memory") →
// field → value, all as the server wrote them. Fields that appear before any
// "# Section" header are under "".
type Info map[string]map[string]string

// ParseInfo parses INFO's text. It never fails: INFO is written for humans,
// every release adds fields, and a scraper that refuses what it does not
// recognise breaks on the next upgrade. So it tolerates CRLF or LF, blank
// lines, headers in any case, lines without a colon (skipped), and values
// that themselves contain colons (split at the first one only —
// "executable:/usr/local/bin/redis-server").
func ParseInfo(s string) Info {
	info := Info{}
	section := ""
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if name, ok := strings.CutPrefix(line, "#"); ok {
			section = strings.ToLower(strings.TrimSpace(name))
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok || field == "" {
			continue
		}
		m := info[section]
		if m == nil {
			m = map[string]string{}
			info[section] = m
		}
		m[field] = value
	}
	return info
}

// Get returns a field's value from whichever section holds it. Field names
// are unique across INFO's sections in every Redis version, which is what
// lets a check ask for "used_memory" without knowing it lives in "memory".
func (i Info) Get(field string) (string, bool) {
	// Sorted so that if two sections ever did share a name, the answer
	// would at least be the same one every time.
	names := make([]string, 0, len(i))
	for name := range i {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if v, ok := i[name][field]; ok {
			return v, true
		}
	}
	return "", false
}

// Number returns a field parsed as a float, and false when it is absent or
// not a number ("1.2M" in used_memory_human, say, or "master" in role).
func (i Info) Number(field string) (float64, bool) {
	v, ok := i.Get(field)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// FieldKind says how a numeric INFO field reads over time.
type FieldKind int

const (
	// Gauge is a level at this instant; report it as it is.
	Gauge FieldKind = iota
	// Counter is a running total since the server started; only its rate
	// between two scrapes means anything, and a decrease is a restart.
	Counter
)

func (k FieldKind) String() string {
	if k == Counter {
		return "counter"
	}
	return "gauge"
}

// Field is one numeric INFO field the redis check reports.
type Field struct {
	Name string
	Kind FieldKind
}

// Fields are the INFO fields the redis check reports, and how to read each.
// Memory, clients and replication are levels; commands, keyspace hits and
// misses, evictions, expirations and rejected connections are running
// totals. instantaneous_ops_per_sec is Redis's own short sample, reported as
// the gauge it is; the ops/s that agrees with the scrape interval is the rate
// of total_commands_processed.
var Fields = []Field{
	{"used_memory", Gauge},
	{"used_memory_rss", Gauge},
	{"maxmemory", Gauge},
	{"mem_fragmentation_ratio", Gauge},
	{"connected_clients", Gauge},
	{"blocked_clients", Gauge},
	{"instantaneous_ops_per_sec", Gauge},
	{"connected_slaves", Gauge},
	{"uptime_in_seconds", Gauge},
	{"total_commands_processed", Counter},
	{"total_connections_received", Counter},
	{"rejected_connections", Counter},
	{"keyspace_hits", Counter},
	{"keyspace_misses", Counter},
	{"evicted_keys", Counter},
	{"expired_keys", Counter},
}

// Role returns the replication role ("master" or "slave"), or "" when INFO
// did not include the replication section.
func (i Info) Role() string {
	v, _ := i.Get("role")
	return strings.TrimSpace(v)
}

// DBStats is one line of INFO's keyspace section.
type DBStats struct {
	DB      int
	Keys    int64
	Expires int64
	// AvgTTL is the average remaining time to live, in milliseconds, of the
	// keys that have one; 0 when none do.
	AvgTTL int64
}

// Keyspace returns the keyspace section's databases, in DB order. A
// database with no keys has no line at all, so an empty result is an empty
// server, not a failure. Pairs other than keys, expires and avg_ttl (Redis 7
// adds subexpiry) are ignored, and a malformed line is skipped.
func (i Info) Keyspace() []DBStats {
	var out []DBStats
	for field, value := range i["keyspace"] {
		num, ok := strings.CutPrefix(field, "db")
		// Digits only, and no leading zero: "db00" is not a database Redis
		// writes, and would otherwise be a second line for DB 0.
		if !ok || num == "" || strings.Trim(num, "0123456789") != "" || (len(num) > 1 && num[0] == '0') {
			continue
		}
		db, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		st, sawKeys := DBStats{DB: db}, false
		for pair := range strings.SplitSeq(strings.TrimSpace(value), ",") {
			k, v, _ := strings.Cut(pair, "=")
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				continue
			}
			switch k {
			case "keys":
				st.Keys, sawKeys = n, true
			case "expires":
				st.Expires = n
			case "avg_ttl":
				st.AvgTTL = n
			}
		}
		if sawKeys {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].DB < out[b].DB })
	return out
}
