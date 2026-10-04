package logstore

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the committed golden files")

// goldenBlocks is the fixture behind testdata/golden/v1.chunk: two blocks,
// the second out of order inside itself and holding an empty body, sealed.
func goldenBlocks() [][]rawEntry {
	return [][]rawEntry{
		{{Ts: 1_790_000_000_000, Seq: 100, Body: []byte(`{"message":"first"}`)}, {Ts: 1_790_000_000_250, Seq: 101, Body: []byte(`{"message":"second","attrs":{"n":1}}`)}},
		{{Ts: 1_790_000_005_000, Seq: 200, Body: []byte(`{"message":"late"}`)}, {Ts: 1_790_000_004_000, Seq: 201, Body: []byte(`{}`)}, {Ts: 1_790_000_004_000, Seq: 202, Body: nil}},
	}
}

// What a golden is for here: the compressed bytes depend on the zstd library's
// version, so this does not compare them. It checks that the committed file,
// written once, still reads: that a future reader can open a v1 chunk, finds
// the same blocks, and decodes the same entries. The bytes that are the
// format's own (the header, the index and the trailer) are checked exactly in
// TestGolden_LayoutBytes. Regenerate with -update-golden and treat needing to
// as a format change that needs a version bump and a note in docs/formats.
func TestGolden_V1ChunkStillReads(t *testing.T) {
	path := filepath.Join("testdata", "golden", "v1.chunk")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		saved := newChunkVersion
		newChunkVersion = 1 // this golden is the v1 layout, whatever new chunks are written in
		built := writeChunk(t, t.TempDir(), goldenBlocks(), true)
		newChunkVersion = saved
		data, _ := os.ReadFile(built)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v — regenerate with: go test ./internal/logstore -run Golden -update-golden", err)
	}
	ix, err := readIndex(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	want := goldenBlocks()
	if !ix.Sealed || len(ix.Blocks) != len(want) {
		t.Fatalf("sealed=%v blocks=%d", ix.Sealed, len(ix.Blocks))
	}
	for i, m := range ix.Blocks {
		got, err := readBlock(bytes.NewReader(data), m)
		if err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		if len(got) != len(want[i]) {
			t.Fatalf("block %d: %d entries, want %d", i, len(got), len(want[i]))
		}
		for j := range got {
			if got[j].Ts != want[i][j].Ts || got[j].Seq != want[i][j].Seq || !bytes.Equal(got[j].Body, want[i][j].Body) {
				t.Errorf("block %d entry %d: %v, want %v", i, j, got[j], want[i][j])
			}
		}
	}
	if ix.Blocks[0].LastSeq != 101 || ix.Blocks[1].LastSeq != 202 {
		t.Errorf("sequence numbers: %d, %d", ix.Blocks[0].LastSeq, ix.Blocks[1].LastSeq)
	}
}

func TestGolden_LayoutBytes(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "golden", "v1.chunk"))
	if err != nil {
		t.Skip("golden missing; TestGolden_V1ChunkStillReads says how to make it")
	}
	if !bytes.Equal(data[:8], []byte{'O', 'Z', 'Y', 'C', 0, 1, 0, 0}) {
		t.Errorf("header = % x", data[:8])
	}
	if !bytes.Equal(data[len(data)-4:], []byte("OZYF")) {
		t.Errorf("trailer magic = %q", data[len(data)-4:])
	}
	// Two index entries (44 bytes each) and the 20-byte trailer end the file.
	if data[len(data)-trailerSize+3] != 2 { // count is a big-endian u32 whose low byte is 2
		t.Errorf("footer count byte = %d, want 2", data[len(data)-trailerSize+3])
	}
}

// goldenLogBlocks is the fixture behind testdata/golden/v2.chunk: two blocks of
// real log bodies, so their filters have something in them.
func goldenLogBlocks() [][]rawEntry {
	body := func(msg string, attrs string) []byte {
		return []byte(`{"ts":1790000000000,"message":"` + msg + `","status":"info","service":"web-api"` + attrs + `}`)
	}
	return [][]rawEntry{
		{{Ts: 1_790_000_000_000, Seq: 1, Body: body("Connection refused by upstream", `,"attrs":{"user":"alice","n":42}`)},
			{Ts: 1_790_000_000_500, Seq: 2, Body: body("request handled", `,"attrs":{"route":"/orders"}`)}},
		{{Ts: 1_790_000_060_000, Seq: 3, Body: body("Épée drawn", `,"attrs":{"ok":true}`)}},
	}
}

func writeChunkWithBlooms(t testing.TB, dir string, blocks [][]rawEntry) string {
	t.Helper()
	path := filepath.Join(dir, "c.chunk")
	w, err := openChunk(path, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, es := range blocks {
		m, comp := mustEncode(t, es)
		if _, err := w.appendBlock(m, comp, buildBloom(es)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.seal(); err != nil {
		t.Fatal(err)
	}
	_ = w.close()
	return path
}

// The v2 golden pins what v2 adds: the 44-byte block header, the 48-byte index
// entry, and the filter bytes themselves, which depend on no compression
// library, only on this repo's tokenizer and hash. If the filter's contents
// change, every existing v2 chunk would answer differently, so that is a format
// change with a version bump, and this test is what says so.
func TestGolden_V2ChunkStillReadsAndItsFiltersAreByteStable(t *testing.T) {
	path := filepath.Join("testdata", "golden", "v2.chunk")
	if *updateGolden {
		data, _ := os.ReadFile(writeChunkWithBlooms(t, t.TempDir(), goldenLogBlocks()))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v — regenerate with: go test ./internal/logstore -run Golden -update-golden", err)
	}
	if !bytes.Equal(data[:8], []byte{'O', 'Z', 'Y', 'C', 0, 2, 0, 0}) {
		t.Errorf("header = % x", data[:8])
	}
	ix, err := readIndex(bytes.NewReader(data), int64(len(data)))
	if err != nil || !ix.Sealed || ix.Version != 2 || len(ix.Blocks) != 2 {
		t.Fatalf("%+v %v", ix, err)
	}
	if got := ix.ValidEnd + int64(2*entrySize(2)+trailerSize); got != int64(len(data)) {
		t.Errorf("a v2 index entry is %d bytes, not 48", entrySize(2))
	}
	for i, m := range ix.Blocks {
		got, err := readBlock(bytes.NewReader(data), m)
		if err != nil || len(got) != len(goldenLogBlocks()[i]) {
			t.Fatalf("block %d: %d entries, %v", i, len(got), err)
		}
		blob := data[m.bloomOffset() : m.bloomOffset()+int64(m.BloomLen)]
		if want := buildBloom(goldenLogBlocks()[i]); !bytes.Equal(blob, want) {
			t.Errorf("block %d: the filter bytes changed:\n got  %x\n want %x", i, blob, want)
		}
		view, ok := readBloom(bytes.NewReader(data), m)
		if !ok {
			t.Fatalf("block %d: the committed filter does not verify", i)
		}
		if i == 0 && (!view.hasLiteral("refused") || !view.hasLiteral("alice") || !view.hasLiteral("/orders")) {
			t.Error("block 0's filter lacks words that are in it")
		}
		if i == 1 && !view.hasLiteral("épée") {
			t.Error("block 1's filter lacks a non-ASCII word that is in it (it is lowercased as the matcher does)")
		}
	}
}
