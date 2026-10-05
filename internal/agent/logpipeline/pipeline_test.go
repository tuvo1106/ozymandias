package logpipeline

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the committed golden files")

var received = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func newPipeline(t testing.TB, spec Spec) (*Pipeline, *testutil.FakeClock) {
	t.Helper()
	clk := testutil.NewFakeClock(received)
	p, err := New(spec, Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	return p, clk
}

func meta() Meta {
	return Meta{Service: "svc", Host: "box", Tags: []string{"env:dev"}, Received: received}
}

// run feeds every line of a fixture through a pipeline and returns what came
// out, flushing at the end so an unfinished Rails request still appears.
func run(t testing.TB, spec Spec, file string, stderr bool) []wire.Log {
	t.Helper()
	p, clk := newPipeline(t, spec)
	f, err := os.Open(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []wire.Log
	sc := bufio.NewScanner(f)
	m := meta()
	m.Stderr = stderr
	for sc.Scan() {
		m.Received = clk.Now()
		out = append(out, p.Process(sc.Text(), m)...)
		clk.Advance(time.Millisecond) // each line arrives a millisecond after the last
	}
	return append(out, p.FlushAll()...)
}

func toJSONL(t testing.TB, logs []wire.Log) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, l := range logs {
		line, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// Every built-in source, over its fixture, against a committed golden of the
// logs it produces. A golden of the whole output is what lets a reviewer read
// what each line became: it is the documentation of the grok patterns.
// Regenerate with -update-golden and read the diff.
func TestGolden_EverySource(t *testing.T) {
	for _, c := range []struct {
		source, file string
		stderr       bool
	}{
		{"python", "python.log", true}, // the python fixture is a container's stderr
		{"winston", "winston.log", false},
		{"postgres", "postgres.log", true},
		{"redis", "redis.log", false},
		{"sidekiq", "sidekiq.log", false},
		{"rails", "rails.log", false},
	} {
		t.Run(c.source, func(t *testing.T) {
			got := toJSONL(t, run(t, Spec{Source: c.source, RateLimit: -1}, c.file, c.stderr))
			golden := filepath.Join("testdata", c.source+".golden.jsonl")
			if *updateGolden {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v — regenerate with: go test ./internal/agent/logpipeline -run Golden -update-golden", err)
			}
			if !bytes.Equal(got, want) {
				gl, wl := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
				for i := 0; i < len(gl) && i < len(wl); i++ {
					if gl[i] != wl[i] {
						t.Fatalf("line %d differs\n got  %s\n want %s", i+1, gl[i], wl[i])
					}
				}
				t.Fatalf("output has %d lines, golden %d", len(gl), len(wl))
			}
		})
	}
}
