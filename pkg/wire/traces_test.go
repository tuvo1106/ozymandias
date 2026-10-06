package wire

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"
)

var updateTraceGolden = flag.Bool("update-trace-golden", false, "rewrite the committed trace vectors that are generated")

var spanNow = time.UnixMicro(1_790_000_000_000_000)

const (
	tid = "0123456789abcdef0123456789abcdef"
	sid = "0123456789abcdef"
)

func span(over string) string {
	base := `{"trace_id":"` + tid + `","span_id":"` + sid + `","parent_id":null,"service":"web-api","name":"http.request",
	 "resource":"GET /api/comics","type":"web","start":1790000000000000,"duration":1500,"error":0,
	 "meta":{"env":"dev","http.status_code":"200"},"metrics":{"_top_level":1}}`
	if over == "" {
		return base
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(base), &m)
	var o map[string]any
	if err := json.Unmarshal([]byte(over), &o); err != nil {
		panic(err)
	}
	for k, v := range o {
		if v == "__delete__" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestDecodeTraces_AcceptsTheDocumentedShape(t *testing.T) {
	body := `{"tracer":{"lang":"python","lang_version":"3.12.4","version":"0.1.0"},"traces":[[` + span("") + `,` +
		span(`{"span_id":"00000000000000aa","parent_id":"`+sid+`","name":"postgres.query","type":"db","metrics":{}}`) + `]]}`
	_, chunks, rejects, err := DecodeTraces([]byte(body), DecodeOptions{Now: spanNow})
	if err != nil || len(rejects) != 0 || len(chunks) != 1 || len(chunks[0]) != 2 {
		t.Fatalf("chunks=%v rejects=%v err=%v", chunks, rejects, err)
	}
	root, child := chunks[0][0], chunks[0][1]
	if root.ParentID != "" || child.ParentID != sid || !root.TopLevel() || child.TopLevel() {
		t.Errorf("root=%+v child=%+v", root, child)
	}
}

func TestDecodeTraces_RejectsEachBadSpanOnItsOwnAndKeepsTheRest(t *testing.T) {
	cases := map[string]string{
		"short trace id":        `{"trace_id":"abc"}`,
		"uppercase trace id":    `{"trace_id":"0123456789ABCDEF0123456789abcdef"}`,
		"zero trace id":         `{"trace_id":"00000000000000000000000000000000"}`,
		"zero span id":          `{"span_id":"0000000000000000"}`,
		"parent is itself":      `{"parent_id":"` + sid + `"}`,
		"bad parent":            `{"parent_id":"xyz"}`,
		"no service":            `{"service":""}`,
		"long service":          `{"service":"` + strings.Repeat("a", 101) + `"}`,
		"no name":               `{"name":""}`,
		"error 2":               `{"error":2}`,
		"negative duration":     `{"duration":-1}`,
		"start in seconds":      `{"start":1790000000}`,
		"start in milliseconds": `{"start":1790000000000}`,
		"future start":          `{"start":1790001000000000}`,
		"zero start":            `{"start":0}`,
		"not an object":         `"hello"`,
	}
	for name, over := range cases {
		item := over
		if strings.HasPrefix(over, "{") {
			item = span(over)
		}
		body := `{"traces":[[` + span("") + `,` + item + `,` + span(`{"span_id":"00000000000000bb"}`) + `]]}`
		_, chunks, rejects, err := DecodeTraces([]byte(body), DecodeOptions{Now: spanNow})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(rejects) != 1 || rejects[0].Chunk != 0 || rejects[0].Index != 1 {
			t.Errorf("%s: rejects = %v", name, rejects)
		}
		if len(chunks) != 1 || len(chunks[0]) != 2 {
			t.Errorf("%s: the good spans were not kept: %v", name, chunks)
		}
	}
}

func TestDecodeTraces_BodyErrors(t *testing.T) {
	for name, body := range map[string]string{"not json": `{`, "no traces": `{"tracer":{}}`, "wrong type": `{"traces":"x"}`} {
		if _, _, _, err := DecodeTraces([]byte(body), DecodeOptions{Now: spanNow}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var sb strings.Builder
	sb.WriteString(`{"traces":[`)
	for i := 0; i <= MaxChunksPerRequest; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("[]")
	}
	sb.WriteString("]}")
	if _, _, _, err := DecodeTraces([]byte(sb.String()), DecodeOptions{Now: spanNow}); err == nil {
		t.Error("too many chunks accepted")
	}
}

func TestNormalizeSpan_CutsTypesLimitsAndSurplus(t *testing.T) {
	long := strings.Repeat("é", 3000) // 6000 bytes
	meta := map[string]string{}
	for i := 0; i < 150; i++ {
		meta[fmt.Sprintf("k%03d", i)] = "v"
	}
	meta["long"] = long
	meta[strings.Repeat("k", 101)] = "dropped: key too long"
	sp := Span{Type: "weird", Resource: long, Meta: meta, Metrics: map[string]float64{"a": 1}}
	NormalizeSpan(&sp)
	if sp.Type != SpanTypeCustom {
		t.Errorf("type = %q", sp.Type)
	}
	if len(sp.Resource) > MaxResourceLen || len(sp.Resource)%2 != 0 { // é is 2 bytes: never split
		t.Errorf("resource is %d bytes", len(sp.Resource))
	}
	if len(sp.Meta) != MaxMetaEntries {
		t.Errorf("%d meta entries kept", len(sp.Meta))
	}
	if _, ok := sp.Meta[strings.Repeat("k", 101)]; ok {
		t.Error("an over-long key survived")
	}
	// Deterministic: the same span always loses the same entries.
	again := Span{Meta: meta}
	NormalizeSpan(&again)
	for k := range sp.Meta {
		if _, ok := again.Meta[k]; !ok {
			t.Fatalf("normalization is not deterministic: %q", k)
		}
	}
}

func TestDecodeSpans_FlatBodyWithEnvAndHost(t *testing.T) {
	p, rejects, err := DecodeSpans([]byte(`{"env":"dev","host":"h1","spans":[`+span("")+`,`+span(`{"service":""}`)+`]}`), DecodeOptions{Now: spanNow})
	if err != nil || p.Env != "dev" || p.Host != "h1" || len(p.Spans) != 1 || len(rejects) != 1 || rejects[0].Chunk != -1 {
		t.Fatalf("%+v %v %v", p, rejects, err)
	}
	if !strings.HasPrefix(rejects[0].String(), "spans[1]:") {
		t.Errorf("%q", rejects[0])
	}
}

func TestSampleKeep_Edges(t *testing.T) {
	for _, c := range []struct {
		rate float64
		want bool
	}{{0, false}, {-1, false}, {1, true}, {2, true}} {
		if SampleKeep(tid, c.rate) != c.want {
			t.Errorf("rate %v", c.rate)
		}
	}
	if SampleKeep("short", 0.5) || SampleKeep(strings.Repeat("z", 32), 0.5) {
		t.Error("a malformed trace id was sampled in")
	}
}

// The decision is a function of the id alone and its keep-fraction is the rate.
func TestSampleKeep_KeepsAboutTheRateAndIsMonotonicInIt(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	ids := make([]string, 20000)
	for i := range ids {
		ids[i] = fmt.Sprintf("%016x%016x", rng.Uint64(), rng.Uint64())
	}
	prev := -1
	for _, rate := range []float64{0.01, 0.1, 0.25, 0.5, 0.9} {
		kept := 0
		for _, id := range ids {
			if SampleKeep(id, rate) {
				kept++
			}
		}
		if got := float64(kept) / float64(len(ids)); got < rate-0.02 || got > rate+0.02 {
			t.Errorf("rate %v kept %.3f", rate, got)
		}
		if kept < prev {
			t.Errorf("a higher rate kept fewer traces")
		}
		prev = kept
	}
	// A trace kept at a low rate is kept at every higher one.
	for _, id := range ids[:2000] {
		if SampleKeep(id, 0.1) && !SampleKeep(id, 0.5) {
			t.Fatalf("%s kept at 0.1 but not 0.5", id)
		}
	}
}

type samplingVectors struct {
	Comment []string `json:"_comment"`
	Cases   []struct {
		TraceID string  `json:"trace_id"`
		Rate    float64 `json:"rate"`
		Keep    bool    `json:"keep"`
	} `json:"cases"`
}

// The shared vectors: Python and Node load this file and must agree with Go on
// every line. Regenerate with -update-trace-golden only if the algorithm changes,
// which would make every service in a mixed fleet disagree about a trace.
func TestSampleKeep_SharedVectors(t *testing.T) {
	path := "testdata/traces/sampling.json"
	if *updateTraceGolden {
		rng := rand.New(rand.NewPCG(7, 9))
		v := samplingVectors{Comment: []string{
			"Head-sampling vectors (docs/wire-protocol.md, 'Sampling'). keep == ((low 64 bits of trace_id) * 1111111111111111111 mod 2^64) < rate * 2^64.",
			"Go (wire.SampleKeep), Python and Node load this same file; generated by `go test ./pkg/wire -update-trace-golden`.",
		}}
		ids := []string{"00000000000000000000000000000001", "ffffffffffffffffffffffffffffffff", "0123456789abcdef0123456789abcdef", "0000000000000000ffffffffffffffff", "ffffffffffffffff0000000000000000", "00000000000000008000000000000000"}
		for i := 0; i < 60; i++ {
			ids = append(ids, fmt.Sprintf("%016x%016x", rng.Uint64(), rng.Uint64()))
		}
		for _, id := range ids {
			for _, rate := range []float64{0, 0.001, 0.1, 0.25, 0.5, 0.75, 0.999, 1} {
				v.Cases = append(v.Cases, struct {
					TraceID string  `json:"trace_id"`
					Rate    float64 `json:"rate"`
					Keep    bool    `json:"keep"`
				}{id, rate, SampleKeep(id, rate)})
			}
		}
		b, _ := json.MarshalIndent(v, "", " ")
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v: regenerate with go test ./pkg/wire -update-trace-golden", err)
	}
	var v samplingVectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	kept, dropped := 0, 0
	for _, c := range v.Cases {
		if got := SampleKeep(c.TraceID, c.Rate); got != c.Keep {
			t.Errorf("SampleKeep(%s, %v) = %v, vector says %v", c.TraceID, c.Rate, got, c.Keep)
		}
		if c.Keep {
			kept++
		} else {
			dropped++
		}
	}
	if kept < 50 || dropped < 50 {
		t.Errorf("the vectors are lopsided (%d kept, %d dropped) and prove little", kept, dropped)
	}
}

func TestNormalizePath_SharedVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/traces/normalize-path.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Cases []struct{ In, Out string } `json:"cases"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Cases {
		if got := NormalizePath(c.In); got != c.Out {
			t.Errorf("NormalizePath(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
	if len(v.Cases) < 20 {
		t.Fatal("too few vectors")
	}
}

func TestParsePropagation(t *testing.T) {
	good := []struct {
		t, p, prio string
		want       int
	}{{tid, sid, "", 1}, {tid, sid, "1", 1}, {tid, sid, "2", 2}, {tid, sid, "0", 0}, {tid, sid, "-1", -1}, {strings.ToUpper(tid), " " + sid + " ", " 1 ", 1}}
	for _, g := range good {
		_, _, prio, ok := ParsePropagation(g.t, g.p, g.prio)
		if !ok || prio != g.want {
			t.Errorf("%+v: ok=%v prio=%d", g, ok, prio)
		}
	}
	for _, bad := range [][3]string{{"", "", ""}, {tid, "", ""}, {"abc", sid, ""}, {tid, sid, "7"}, {tid, sid, "high"}, {strings.Repeat("0", 32), sid, ""}, {tid, strings.Repeat("0", 16), ""}, {tid + "00", sid, ""}} {
		if _, _, _, ok := ParsePropagation(bad[0], bad[1], bad[2]); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func FuzzDecodeTraces(f *testing.F) {
	f.Add([]byte(`{"tracer":{},"traces":[[` + span("") + `]]}`))
	f.Add([]byte(`{"traces":[[{"trace_id":1}]]}`))
	f.Add([]byte(`{"traces":[null,[null]]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, chunks, _, err := DecodeTraces(b, DecodeOptions{Now: spanNow})
		if err != nil {
			return
		}
		for _, c := range chunks {
			for i := range c {
				if err := ValidateSpan(&c[i], DecodeOptions{Now: spanNow}); err != nil {
					t.Fatalf("a span DecodeTraces returned does not validate: %v", err)
				}
			}
		}
	})
}

func FuzzParsePropagation(f *testing.F) {
	f.Add(tid, sid, "1")
	f.Fuzz(func(t *testing.T, a, b, c string) {
		tidv, pid, _, ok := ParsePropagation(a, b, c)
		if ok && (len(tidv) != 32 || len(pid) != 16) {
			t.Fatalf("ok with malformed ids %q %q", tidv, pid)
		}
	})
}
