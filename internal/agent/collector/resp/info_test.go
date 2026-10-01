package resp

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func golden(t *testing.T, name string) Info {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return ParseInfo(string(b))
}

// The goldens are synthetic INFO outputs shaped like Redis 6.2 and 7.2's,
// with CRLF line endings as a server sends them. Every field the check reads
// must come out of both.
func TestParseInfo_Goldens(t *testing.T) {
	for _, tc := range []struct {
		file   string
		want   map[string]float64
		role   string
		dbs    []DBStats
		absent []string
	}{
		{
			file: "info-redis6.txt",
			want: map[string]float64{
				"used_memory": 1048576, "used_memory_rss": 4194304, "maxmemory": 0,
				"mem_fragmentation_ratio": 4, "connected_clients": 12, "blocked_clients": 2,
				"instantaneous_ops_per_sec": 17, "total_commands_processed": 90210,
				"total_connections_received": 150, "keyspace_hits": 5000, "keyspace_misses": 250,
				"evicted_keys": 0, "expired_keys": 41, "rejected_connections": 0,
				"uptime_in_seconds": 86400, "connected_slaves": 0,
			},
			role: "master",
			dbs:  []DBStats{{DB: 0, Keys: 120, Expires: 10, AvgTTL: 3600000}, {DB: 3, Keys: 7}},
		},
		{
			file: "info-redis7.txt",
			want: map[string]float64{
				"used_memory": 2097152, "used_memory_rss": 8388608, "maxmemory": 104857600,
				"mem_fragmentation_ratio": 3.95, "connected_clients": 3, "blocked_clients": 0,
				"instantaneous_ops_per_sec": 0, "total_commands_processed": 1234,
				"total_connections_received": 9, "keyspace_hits": 10, "keyspace_misses": 90,
				"evicted_keys": 25, "expired_keys": 0, "rejected_connections": 1,
				"uptime_in_seconds": 3600, "connected_slaves": 0,
			},
			role: "slave",
			// subexpiry is new in 7 and ignored.
			dbs: []DBStats{{DB: 0, Keys: 42, Expires: 2, AvgTTL: 15000}},
		},
	} {
		info := golden(t, tc.file)
		for name, want := range tc.want {
			if got, ok := info.Number(name); !ok || got != want {
				t.Errorf("%s: %s = %v (found %v), want %v", tc.file, name, got, ok, want)
			}
		}
		if r, _ := info.Get("role"); r != tc.role {
			t.Errorf("%s: role = %q", tc.file, r)
		}
		if got := info.Keyspace(); !reflect.DeepEqual(got, tc.dbs) {
			t.Errorf("%s: keyspace = %+v, want %+v", tc.file, got, tc.dbs)
		}
		// No value keeps the CR of its CRLF.
		for section, fields := range info {
			for k, v := range fields {
				if strings.ContainsAny(k+v, "\r\n") {
					t.Errorf("%s: [%s] %q=%q kept a line ending", tc.file, section, k, v)
				}
			}
		}
	}
}

func TestParseInfo_Tolerance(t *testing.T) {
	info := ParseInfo(strings.Join([]string{
		"preamble:1", // before any header
		"#   MEMORY  ",
		"used_memory:5",
		"",
		"   ",
		"no colon here",
		":no field name",
		"executable:/usr/local/bin/redis-server", // colons in the value
		"config_file:",                           // empty value
		"used_memory_human:5B",
		"# Keyspace",
		"db0:keys=3,expires=1,avg_ttl=9,future=7",
		"db1:garbage",           // no keys= → skipped
		"dbx:keys=1",            // not a database number
		"db:keys=1",             // no number at all
		"db+2:keys=1",           // a sign is not a number here
		"db10:keys=2,expires=z", // a bad pair is ignored, the rest kept
		"somethingelse:keys=1",
	}, "\n"))
	if v, _ := info.Get("preamble"); v != "1" {
		t.Errorf("preamble = %q, want it under the empty section", v)
	}
	if _, ok := info["memory"]; !ok {
		t.Errorf("sections = %v, want the header lower-cased and trimmed", info)
	}
	if n, ok := info.Number("used_memory"); !ok || n != 5 {
		t.Errorf("used_memory = %v, %v", n, ok)
	}
	if v, _ := info.Get("executable"); v != "/usr/local/bin/redis-server" {
		t.Errorf("executable = %q", v)
	}
	if v, ok := info.Get("config_file"); !ok || v != "" {
		t.Errorf("config_file = %q, %v", v, ok)
	}
	if _, ok := info.Number("used_memory_human"); ok {
		t.Error("5B is not a number")
	}
	if _, ok := info.Number("missing"); ok {
		t.Error("a missing field is not a number")
	}
	if _, ok := info.Get(""); ok {
		t.Error("a line with no field name was kept")
	}
	want := []DBStats{{DB: 0, Keys: 3, Expires: 1, AvgTTL: 9}, {DB: 10, Keys: 2}}
	if got := info.Keyspace(); !reflect.DeepEqual(got, want) {
		t.Errorf("keyspace = %+v, want %+v", got, want)
	}
	if _, ok := ParseInfo("").Get("role"); ok || len(ParseInfo("").Keyspace()) != 0 {
		t.Error("an empty INFO has no role and no databases")
	}
}

func FuzzParseInfo(f *testing.F) {
	for _, name := range []string{"info-redis6.txt", "info-redis7.txt"} {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(string(b))
	}
	f.Add("# Keyspace\ndb0:keys=1,expires=0,avg_ttl=0\ndb1:keys=-,x\n")
	f.Fuzz(func(t *testing.T, s string) {
		info := ParseInfo(s)
		for _, name := range []string{"used_memory", "mem_fragmentation_ratio", "role", "keyspace_hits"} {
			info.Number(name)
		}
		dbs := info.Keyspace()
		for i := 1; i < len(dbs); i++ {
			if dbs[i-1].DB >= dbs[i].DB {
				t.Fatalf("keyspace not in strict DB order: %+v", dbs)
			}
		}
		for section, fields := range info {
			if strings.Contains(section, "\n") {
				t.Fatalf("section %q spans lines", section)
			}
			for k := range fields {
				if k == "" || strings.ContainsAny(k, ":\n") {
					t.Fatalf("field %q", k)
				}
			}
		}
	})
}

// A field two sections share is answered by the section whose name sorts
// first, every time, whatever the map's iteration order.
func TestInfo_GetIsStableWhenSectionsShareAField(t *testing.T) {
	info := ParseInfo("# Zeta\r\nx:z\r\n# alpha\r\nx:a\r\n# mid\r\nx:m\r\n")
	for range 50 {
		if v, ok := info.Get("x"); !ok || v != "a" {
			t.Fatalf("Get = %q, %v; want the alpha section's", v, ok)
		}
	}
}
