package logstore

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/query/logql"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

func bodyOf(t tb, l wire.Log) rawEntry {
	t.Helper()
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	return rawEntry{Ts: l.Ts, Body: b}
}

func bloomOfLogs(t tb, logs ...wire.Log) bloomView {
	t.Helper()
	var es []rawEntry
	for i, l := range logs {
		e := bodyOf(t, l)
		e.Seq = uint64(i + 1)
		es = append(es, e)
	}
	b, ok := parseBloom(buildBloom(es))
	if !ok {
		t.Fatal("a freshly built filter does not verify")
	}
	return b
}

// The one property that makes a bloom filter safe to skip blocks with: a log
// that matches a query is never in a block the filter rules out. Checked
// against the real matcher (logql.Compile), over text with case, wildcards,
// non-ASCII, numbers, booleans, arrays and nesting.
func TestBloom_NeverRulesOutABlockHoldingAMatch(t *testing.T) {
	words := []string{"timeout", "Refused", "épée", "ÉPÉE", "alpha-beta", "x", "ab", "gamma_delta", "Zoë", "12345", "1e3", "true", "/api/v1/items", "a.b.c"}
	queries := []string{
		"timeout", "TIMEOUT", "refus*", "*fused", "t*e*t", "épée", "ÉPÉE", "alpha-b*", "gamma_d*a", "zoë", "123", "12345", "1e3", "1000",
		"/api/v1", "a.b", "ab", "x", "*", "**", "true", "-timeout", "timeout OR épée", "timeout refused", "(timeout OR zoë) refus*",
		"@user:zoë", "@user:Z*", "@user:*", "@n:12345", "@n:1000", "@n:>5", "@n:1e3", "@tags:épée", "@o.p:alpha-beta", "@ok:true",
	}
	rapid.Check(t, func(t *rapid.T) {
		pick := func(label string) string { return rapid.SampledFrom(words).Draw(t, label) }
		var logs []wire.Log
		for i := 0; i < rapid.IntRange(1, 8).Draw(t, "n"); i++ {
			l := wire.Log{Ts: 1, Service: "a", Status: "info", Message: pick(fmt.Sprint("m1", i)) + " " + pick(fmt.Sprint("m2", i)),
				Attrs: map[string]any{
					"user": pick(fmt.Sprint("u", i)), "n": json.Number(rapid.SampledFrom([]string{"12345", "1e3", "1000", "7"}).Draw(t, fmt.Sprint("nn", i))),
					"tags": []any{pick(fmt.Sprint("t", i))}, "o": map[string]any{"p": pick(fmt.Sprint("op", i))}, "ok": rapid.Bool().Draw(t, fmt.Sprint("ok", i)),
				}}
			logs = append(logs, l)
		}
		var es []rawEntry
		for i, l := range logs {
			e := bodyOf(t, l)
			e.Seq = uint64(i + 1)
			es = append(es, e)
		}
		view, ok := parseBloom(buildBloom(es))
		if !ok {
			t.Fatal("filter does not verify")
		}
		for _, q := range queries {
			node, err := logql.Parse(q)
			if err != nil {
				t.Fatalf("%q: %v", q, err)
			}
			f, need := logql.Compile(node), logql.Needs(node)
			for _, l := range logs {
				l := l
				// The matcher sees what the store would hand it: the log as decoded from JSON.
				dec, _ := decodeLog(bodyOf(t, l).Body)
				if f(dec) && !view.mayMatch(need) {
					t.Fatalf("query %q matches %+v but its block was ruled out (need %v)", q, l, need.Alts)
				}
			}
		}
	})
}

func TestBloom_WhatGoesInAndWhatDoesNot(t *testing.T) {
	v := bloomOfLogs(t, wire.Log{Ts: 1, Service: "svc-label", Status: "info", Host: "hostname-label", Message: "Connection REFUSED",
		Attrs: map[string]any{"attrname": "valuetext", "n": json.Number("98765"), "b": true, "nested": map[string]any{"k": []any{"inner"}}}})
	for _, in := range []string{"connection", "refused", "valuetext", "98765", "true", "inner", "ecti"} {
		if !v.hasLiteral(in) {
			t.Errorf("%q should be in the filter", in)
		}
	}
	// Not in it (and, for a 3-byte window, absent with overwhelming probability at this size).
	for _, out := range []string{"svc-label", "hostname-label", "attrname"} {
		if v.hasLiteral(out) {
			t.Errorf("%q is a label or an attribute name and must not be in the filter", out)
		}
	}
}

func TestBloom_FalsePositiveRateIsAboutWhatItIsSizedFor(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	alpha := "abcdefghijklmnopqrstuvwxyz0123456789 -_/."
	text := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alpha[rng.IntN(len(alpha))]
		}
		return string(b)
	}
	var es []rawEntry
	present := map[uint32]bool{}
	for i := 0; i < 400; i++ {
		msg := text(120)
		for j := 0; j+2 < len(msg); j++ {
			present[trigram(msg[j], msg[j+1], msg[j+2])] = true
		}
		e := bodyOf(t, wire.Log{Ts: 1, Service: "a", Status: "info", Message: msg})
		e.Seq = uint64(i + 1)
		es = append(es, e)
	}
	v, _ := parseBloom(buildBloom(es))
	fp, trials := 0, 0
	for a := 0; a < 40; a++ {
		for b := 0; b < 40; b++ {
			for c := 0; c < 40; c++ {
				tri := trigram(alpha[a], alpha[b], alpha[c])
				if present[tri] {
					continue
				}
				trials++
				if v.has(tri) {
					fp++
				}
			}
		}
	}
	rate := float64(fp) / float64(trials)
	t.Logf("%d distinct windows, filter %d bytes, false positive rate %.2f%% over %d absent windows", len(present), len(v.bits), rate*100, trials)
	if trials < 1000 || rate > 0.06 {
		t.Fatalf("false positive rate %.3f over %d trials; sized for about 2.4%%", rate, trials)
	}
}

func TestBloom_ADamagedFilterIsIgnoredNotTrusted(t *testing.T) {
	blob := buildBloom([]rawEntry{bodyOf(t, wire.Log{Ts: 1, Service: "a", Status: "info", Message: "hello world"})})
	if _, ok := parseBloom(blob); !ok {
		t.Fatal("good filter rejected")
	}
	for i := range blob {
		for bit := 0; bit < 8; bit++ {
			bad := append([]byte(nil), blob...)
			bad[i] ^= 1 << bit
			if _, ok := parseBloom(bad); ok {
				t.Fatalf("a flipped bit at byte %d bit %d verified: a bit that turns 1 into 0 would deny a log that is there", i, bit)
			}
		}
	}
	for _, short := range [][]byte{nil, {4}, blob[:5], blob[:len(blob)-1]} {
		if _, ok := parseBloom(short); ok {
			t.Errorf("a %d-byte blob verified", len(short))
		}
	}
	// A filter built with another hash count has a valid checksum and would answer
	// "absent" for windows that are there: it must be refused, not probed with ours.
	other := append([]byte(nil), blob...)
	other[0] = bloomK + 1
	binary.BigEndian.PutUint32(other[len(other)-bloomTrailerBytes:], crc32.Checksum(other[:len(other)-bloomTrailerBytes], castagnoli))
	if _, ok := parseBloom(other); ok {
		t.Error("a filter with an unknown hash count verified")
	}
}

func TestBloom_SizeFollowsDistinctWindowsAndIsCapped(t *testing.T) {
	small := buildBloom([]rawEntry{bodyOf(t, wire.Log{Ts: 1, Service: "a", Status: "info", Message: "ab"})})
	if len(small) != 1+minBloomBytes+bloomTrailerBytes {
		t.Errorf("a block with no windows got %d bytes", len(small))
	}
	var es []rawEntry
	rng := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < 600; i++ {
		b := make([]byte, 400)
		for j := range b {
			b[j] = byte('a' + rng.IntN(26))
		}
		e := bodyOf(t, wire.Log{Ts: 1, Service: "a", Status: "info", Message: string(b)})
		e.Seq = uint64(i)
		es = append(es, e)
	}
	big := buildBloom(es) // ~17k distinct windows would want 17kB; random text has more than the cap allows
	if len(big) > 1+maxBloomBytes+bloomTrailerBytes {
		t.Errorf("filter of %d bytes exceeds the cap", len(big))
	}
	if buildBloom([]rawEntry{{Ts: 1, Seq: 1, Body: []byte("not json")}}) != nil {
		t.Error("an undecodable body must yield no filter, not a wrong one")
	}
}

// --- through the store ---

func logsWithNeedle(day time.Time, blocks, perBlock int) ([][]wire.Log, string) {
	needle := "quokka-7731"
	var out [][]wire.Log
	for b := 0; b < blocks; b++ {
		var batch []wire.Log
		for i := 0; i < perBlock; i++ {
			l := wire.Log{Ts: day.Add(time.Duration(b*perBlock+i) * time.Second).UnixMilli(), Service: "api", Status: "info",
				Message: fmt.Sprintf("request %d handled ok in block %d", i, b), Attrs: map[string]any{"user": fmt.Sprintf("user%d", i%5)}}
			if b == blocks/2 && i == 3 {
				l.Message += " " + needle
			}
			batch = append(batch, l)
		}
		out = append(out, batch)
	}
	return out, needle
}

func TestSearch_ABloomFilterSkipsBlocksThatCannotMatch(t *testing.T) {
	s, _ := openStore(t, t.TempDir())
	defer func() { _ = s.Close() }()
	batches, needle := logsWithNeedle(t0, 10, 40)
	for _, b := range batches {
		if err := s.Append(context.Background(), b); err != nil {
			t.Fatal(err)
		}
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.Search(context.Background(), mustParse(t, needle), wideFrom, wideTo, SearchOpts{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Logs) != 1 || !strings.Contains(res.Logs[0].Message, needle) {
		t.Fatalf("%d logs", len(res.Logs))
	}
	if res.Stats.BlocksRead != 1 || res.Stats.BlocksSkipped != 9 {
		t.Fatalf("read %d blocks and skipped %d; want 1 and 9: only the block holding the needle should be decompressed", res.Stats.BlocksRead, res.Stats.BlocksSkipped)
	}
	// A skipped block costs no scan budget, so a needle in a haystack the budget
	// could never have covered is still found.
	one := res.Stats.BytesRead // exactly the one block
	res2, _ := s.Search(context.Background(), mustParse(t, needle), wideFrom, wideTo, SearchOpts{Limit: 100, ScanBudget: one})
	if res2.Truncated || len(res2.Logs) != 1 {
		t.Fatalf("truncated=%v with %d logs: skipped blocks must not spend the budget", res2.Truncated, len(res2.Logs))
	}
	// Aggregates and facets skip the same way.
	agg, err := s.Aggregate(context.Background(), mustParse(t, needle), wideFrom, wideTo, AggSpec{Interval: time.Hour})
	if err != nil || agg.Stats.BlocksSkipped != 9 {
		t.Fatalf("aggregate skipped %d: %v", agg.Stats.BlocksSkipped, err)
	}
	// A query the filter cannot speak to reads everything, as before.
	all, _ := s.Search(context.Background(), mustParse(t, "ok"), wideFrom, wideTo, SearchOpts{Limit: 1000})
	if all.Stats.BlocksSkipped != 0 || len(all.Logs) != 400 {
		t.Fatalf("a word in every block: skipped %d, %d logs", all.Stats.BlocksSkipped, len(all.Logs))
	}
	// An OR rules out a block only if every arm does.
	both, _ := s.Search(context.Background(), mustParse(t, needle+" OR block*7"), wideFrom, wideTo, SearchOpts{Limit: 1000})
	if len(both.Logs) != 41 {
		t.Fatalf("OR returned %d logs, want 41", len(both.Logs))
	}
}

// A filter that fails its checksum must make the block be read, never skipped.
func TestSearch_ACorruptBloomFilterFallsBackToReadingTheBlock(t *testing.T) {
	dir := t.TempDir()
	s, _ := openStore(t, dir)
	batches, needle := logsWithNeedle(t0, 4, 30)
	for _, b := range batches {
		_ = s.Append(context.Background(), b)
		_ = s.Flush()
	}
	_ = s.Close()
	// Flip a byte inside every block's filter: each now fails its checksum.
	for _, path := range findFiles(t, dir, ".chunk") {
		data, _ := os.ReadFile(path)
		ix, err := readIndex(bytesReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ix.Blocks {
			if m.BloomLen == 0 {
				t.Fatal("no filter was written")
			}
			data[m.bloomOffset()+3] ^= 0xff
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, _ = openStore(t, dir)
	defer func() { _ = s.Close() }()
	res, err := s.Search(context.Background(), mustParse(t, needle), wideFrom, wideTo, SearchOpts{Limit: 10})
	if err != nil || len(res.Logs) != 1 {
		t.Fatalf("%d logs, %v: a damaged filter lost a log", len(res.Logs), err)
	}
	if res.Stats.BlocksSkipped != 0 || res.Stats.BlocksRead != 4 {
		t.Fatalf("skipped %d, read %d: damaged filters must be ignored", res.Stats.BlocksSkipped, res.Stats.BlocksRead)
	}
}

// A day written by an older binary (v1, no filters) is read as before, and the
// next day's chunk, written by this one, has filters; one query reads both.
func TestStore_V1ChunksStayReadableBesideV2(t *testing.T) {
	dir := t.TempDir()
	day1 := t0.Add(-48 * time.Hour)
	batches, needle := logsWithNeedle(day1, 3, 20)

	saved := newChunkVersion
	newChunkVersion = 1 // this binary writes the old format for a moment
	s, _ := openStoreAt(t, dir, day1)
	for _, b := range batches {
		_ = s.Append(context.Background(), b)
		_ = s.Flush()
	}
	_ = s.Close()
	newChunkVersion = saved

	s, _ = openStore(t, dir)
	defer func() { _ = s.Close() }()
	more, needle2 := logsWithNeedle(t0, 3, 20)
	for _, b := range more {
		_ = s.Append(context.Background(), b)
		_ = s.Flush()
	}
	var v1, v2 int
	for _, path := range findFiles(t, dir, ".chunk") {
		data, _ := os.ReadFile(path)
		ix, _ := readIndex(bytesReader(data), int64(len(data)))
		if ix.Version == 1 {
			v1++
		} else {
			v2++
		}
	}
	if v1 != 1 || v2 != 1 {
		t.Fatalf("%d v1 and %d v2 chunks; want one of each", v1, v2)
	}
	for _, q := range []string{needle, needle2, "request"} {
		res, err := s.Search(context.Background(), mustParse(t, q), wideFrom, wideTo, SearchOpts{Limit: 1000})
		want := 120
		if q != "request" {
			want = 2 // the needle string is the same in both days
		}
		if err != nil || len(res.Logs) != want {
			t.Fatalf("%q: %d logs (want %d), %v", q, len(res.Logs), want, err)
		}
	}
	res, _ := s.Search(context.Background(), mustParse(t, needle), wideFrom, wideTo, SearchOpts{Limit: 10})
	// The v2 day (3 blocks, one with the needle) skips 2; the v1 day has no filters and reads all 3.
	if res.Stats.BlocksSkipped != 2 || res.Stats.BlocksRead != 4 {
		t.Fatalf("skipped %d and read %d; want 2 skipped and 4 read: %+v", res.Stats.BlocksSkipped, res.Stats.BlocksRead, res.Stats)
	}
}

// A v1 chunk that a v2 binary appends to stays v1 (no filters), so one file
// never mixes block layouts.
func TestChunk_AV1FileKeepsItsVersionWhenAppendedTo(t *testing.T) {
	dir := t.TempDir()
	saved := newChunkVersion
	newChunkVersion = 1
	path := writeChunk(t, dir, [][]rawEntry{entries(5, 1000)}, true)
	newChunkVersion = saved
	w, err := openChunk(path, false)
	if err != nil {
		t.Fatal(err)
	}
	m, comp := mustEncode(t, entries(5, 2000))
	pub, err := w.appendBlock(m, comp, []byte("a filter a v1 layout has no room for"))
	if err != nil || pub.BloomLen != 0 {
		t.Fatalf("v1 chunk took a filter: %+v %v", pub, err)
	}
	_ = w.seal()
	_ = w.close()
	data, ix := readAll(t, path)
	if ix.Version != 1 || len(ix.Blocks) != 2 || !ix.Sealed {
		t.Fatalf("%+v", ix)
	}
	for _, b := range ix.Blocks {
		if es, err := readBlock(bytesReader(data), b); err != nil || len(es) != 5 {
			t.Fatalf("%v", err)
		}
	}
	_ = filepath.Join
}
