package openmetrics

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the committed golden files")

// TestGolden parses canned pages shaped like real exporters' output —
// node_exporter, redis_exporter, a Rails app via yabeda, a Go app via
// client_golang, and an OpenMetrics page with every family type — and
// compares a readable dump of the result with the committed .golden file.
// The pages are synthetic: no real hostnames, addresses or identifiers.
//
// Regenerate with `go test ./internal/agent/collector/openmetrics -update-golden`
// and read the diff: a golden that changes is a parser that changed.
func TestGolden(t *testing.T) {
	pages, err := filepath.Glob("testdata/*.prom")
	if err != nil {
		t.Fatal(err)
	}
	om, _ := filepath.Glob("testdata/*.om")
	pages = append(pages, om...)
	if len(pages) < 5 {
		t.Fatalf("found %d pages, want at least 5", len(pages))
	}
	for _, page := range pages {
		t.Run(filepath.Base(page), func(t *testing.T) {
			body, err := os.ReadFile(page)
			if err != nil {
				t.Fatal(err)
			}
			fams, err := Parse(bytes.NewReader(body), Options{})
			if err != nil {
				t.Fatal(err)
			}
			got := dump(t, fams)
			golden := strings.TrimSuffix(page, filepath.Ext(page)) + ".golden"
			if *updateGolden {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update-golden to create it)", err)
			}
			if got != string(want) {
				t.Fatalf("%s differs from %s; rerun with -update-golden and review the diff.\ngot:\n%s", page, golden, got)
			}
		})
	}
}

// dump renders families one line per sample, plus each histogram's grouped
// buckets, in a form a reviewer can read in a diff.
func dump(t *testing.T, fams []Family) string {
	var b strings.Builder
	for _, f := range fams {
		fmt.Fprintf(&b, "family %s type=%s", f.Name, f.Type)
		if f.Unit != "" {
			fmt.Fprintf(&b, " unit=%s", f.Unit)
		}
		if f.Help != "" {
			fmt.Fprintf(&b, " help=%s", strconv.Quote(f.Help))
		}
		b.WriteByte('\n')
		for _, s := range f.Samples {
			fmt.Fprintf(&b, "  %s%s %s", s.Name, dumpLabels(s.Labels), strconv.FormatFloat(s.Value, 'g', -1, 64))
			if s.HasTimestamp {
				fmt.Fprintf(&b, " @%dms", s.TimestampMs)
			}
			b.WriteByte('\n')
		}
		if f.Type == TypeHistogram || f.Type == TypeGaugeHistogram {
			hs, err := f.Histograms()
			if err != nil {
				t.Fatalf("%s: %v", f.Name, err)
			}
			for _, h := range hs {
				fmt.Fprintf(&b, "  histogram%s sum=%v(%v) count=%v(%v)", dumpLabels(h.Labels), h.Sum, h.HasSum, h.Count, h.HasCount)
				for _, bk := range h.Buckets {
					fmt.Fprintf(&b, " [≤%v:%v]", bk.UpperBound, bk.Count)
				}
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

func dumpLabels(ls []Label) string {
	if len(ls) == 0 {
		return ""
	}
	parts := make([]string, len(ls))
	for i, l := range ls {
		parts[i] = l.Name + "=" + strconv.Quote(l.Value)
	}
	return "{" + strings.Join(parts, ",") + "}"
}
