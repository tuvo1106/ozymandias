package spec

import "testing"

// FuzzParseQuery holds the monitor query parser to two rules on arbitrary
// input: it never panics (it runs on whatever a POST to the API carries), and
// whatever it accepts it accepts again after printing, unchanged — the
// canonical form the API echoes must itself be a valid monitor query.
func FuzzParseQuery(f *testing.F) {
	for _, s := range []string{
		"avg(last_5m):avg:jobs.queue.depth{service:api} > 20",
		"max(last_1h):p95:trace.http.request.duration{service:api} by {resource} >= 1.5",
		"pct_nonnull(last_3m):avg:heartbeat{*} < 50",
		`logs("service:worker status:error").rollup("count").last("5m") > 5`,
		`logs("a \"b\"").rollup("count").by("@http.status").last("2h") != 0`,
		"avg(last_5m):avg:x{route:/a>b} > 1",
		"avg(last_5m):",
		`logs("x").rollup("count").by("a","b").last("5m") > 1`,
		"",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		q, err := ParseQuery(src)
		if err != nil {
			return
		}
		printed := q.String()
		again, err := ParseQuery(printed)
		if err != nil {
			t.Fatalf("%q printed as %q, which does not parse: %v", src, printed, err)
		}
		if again.String() != printed {
			t.Fatalf("%q: printing is not stable: %q then %q", src, printed, again.String())
		}
		if again.Op != q.Op || again.Threshold != q.Threshold || again.Window != q.Window || again.Grouped() != q.Grouped() {
			t.Fatalf("%q: round trip changed the meaning: %+v vs %+v", src, q, again)
		}
	})
}

// FuzzCompileTemplate: an arbitrary message never panics at compile, and one
// that compiles renders for every status without error and within the bound
// — compile is the promise that a saved monitor's message can be sent.
func FuzzCompileTemplate(f *testing.F) {
	for _, s := range []string{
		"{{.Monitor}} is {{.Status}} at {{.Value}} on {{.Group}} @ops",
		"{{if .IsAlert}}page{{else if .IsRecovery}}ok{{end}}",
		`{{printf "%.2f" .Value}} {{.Group.service}} {{.Since.Format "15:04"}}`,
		"{{range 3}}x{{end}}",
		`{{printf "%099999d" 1}}`,
		"{{",
		"{{.Link}}{{len .Group}}{{index .Group \"k\"}}",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > MaxMessageBytes {
			return
		}
		tm, err := CompileTemplate(src)
		if err != nil {
			return
		}
		for _, d := range samples() {
			out, err := tm.Render(d)
			if err != nil {
				t.Fatalf("%q compiled but failed to render %s: %v", src, d.Status, err)
			}
			if len(out) > MaxRenderedBytes {
				t.Fatalf("%q rendered %d bytes", src, len(out))
			}
		}
	})
}
