package statsd

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the .golden interpretations")

// The compatibility captures are what real extended StatsD clients actually put on
// the wire (pkg/wire/testdata/statsd-compat/, refreshed by
// scripts/capture-statsd-compat.sh). Our reading of the spec is not the
// contract — their output is, because those are the clients an instrumented
// app will reach for.
//
// The .golden file next to each capture records how we *interpret* every line.
// A diff there is the interesting event: it means a client changed its output
// or we changed our parsing, and either way somebody should look. Asserting
// only "it parses" would miss the failures that matter, like a container id
// silently becoming a tag.
func TestParse_ThirdPartyClientCaptures(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "pkg", "wire", "testdata", "statsd-compat")
	captures, err := filepath.Glob(filepath.Join(dir, "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	for _, path := range captures {
		if filepath.Base(path) == "VERSIONS.txt" {
			continue
		}
		seen++
		t.Run(strings.TrimSuffix(filepath.Base(path), ".txt"), func(t *testing.T) {
			var out strings.Builder
			var metrics, events, checks int
			for _, line := range captureLines(t, path) {
				m, err := Parse([]byte(line))
				switch {
				case errors.Is(err, ErrEvent):
					events++
					fmt.Fprintf(&out, "%-64s → dropped (event)\n", line)
					continue
				case errors.Is(err, ErrServiceCheck):
					checks++
					fmt.Fprintf(&out, "%-64s → dropped (service check)\n", line)
					continue
				case err != nil:
					t.Errorf("a real client sent a line we cannot parse:\n  %s\n  %v", line, err)
					continue
				}
				metrics++
				fmt.Fprintf(&out, "%-64s → %s\n", line, render(&m))

				// Invariants worth stating outright, not just snapshotting:
				// the extension fields must vanish, not leak into a tag.
				m.EachTag(func(tag []byte) {
					if k, _, ok := strings.Cut(string(tag), ":"); ok && (k == "c" || k == "card") {
						t.Errorf("%s: extension field leaked in as tag %q", line, tag)
					}
				})
			}
			if metrics == 0 || events == 0 || checks == 0 {
				t.Errorf("capture is not exercising the format: %d metrics, %d events, %d checks",
					metrics, events, checks)
			}
			compareGolden(t, strings.TrimSuffix(path, ".txt")+".golden", out.String())
		})
	}
	if seen < 2 {
		t.Fatalf("expected captures from at least two clients, found %d", seen)
	}
}

// render describes a parsed message the way a human would check it.
func render(m *Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", m.Name, m.Type)
	if m.SetMember != nil {
		fmt.Fprintf(&b, " member=%s", m.SetMember)
	} else {
		fmt.Fprintf(&b, " value=%g", m.Value)
	}
	if m.SampleRate != 1 {
		fmt.Fprintf(&b, " rate=%g", m.SampleRate)
	}
	if m.Timestamp != 0 {
		fmt.Fprintf(&b, " ts=%d", m.Timestamp)
	}
	var tags []string
	m.EachTag(func(tag []byte) { tags = append(tags, string(tag)) })
	if len(tags) > 0 {
		fmt.Fprintf(&b, " tags=[%s]", strings.Join(tags, " "))
	}
	return b.String()
}

// captureLines returns a capture's metric lines, dropping the '#' commentary
// the capture script writes around them.
func captureLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 {
		t.Fatalf("%s has no metric lines", path)
	}
	return lines
}

func compareGolden(t *testing.T, path, got string) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (regenerate with: go test ./internal/agent/statsd -run Captures -update)", err)
	}
	if string(want) != got {
		t.Errorf("interpretation changed; rerun with -update after checking the diff\n--- want\n%s\n--- got\n%s", want, got)
	}
}

// The multi-line datagram in every capture is the batching an SDK does. The
// server splits on newlines, so it has to survive the real thing.
func TestLines_BatchedCaptureDatagram(t *testing.T) {
	batch := "app.batched:1|c|#i:0\napp.batched:1|c|#i:1\napp.batched:1|c|#i:2\napp.batched:1|c|#i:3"
	var n int
	Lines([]byte(batch), func(line []byte) {
		m, err := Parse(line)
		if err != nil {
			t.Fatalf("line %d: %v", n, err)
		}
		if got, want := string(m.Name), "app.batched"; got != want {
			t.Errorf("name = %s, want %s", got, want)
		}
		n++
	})
	if n != 4 {
		t.Errorf("split into %d lines, want 4", n)
	}
}
