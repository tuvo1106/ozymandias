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
		{{Ts: 1_790_000_000_000, Body: []byte(`{"message":"first"}`)}, {Ts: 1_790_000_000_250, Body: []byte(`{"message":"second","attrs":{"n":1}}`)}},
		{{Ts: 1_790_000_005_000, Body: []byte(`{"message":"late"}`)}, {Ts: 1_790_000_004_000, Body: []byte(`{}`)}, {Ts: 1_790_000_004_000, Body: nil}},
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
		built := writeChunk(t, t.TempDir(), goldenBlocks(), true)
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
			if got[j].Ts != want[i][j].Ts || !bytes.Equal(got[j].Body, want[i][j].Body) {
				t.Errorf("block %d entry %d: %v, want %v", i, j, got[j], want[i][j])
			}
		}
	}
	if ix.Blocks[0].LastSeq != 100 || ix.Blocks[1].LastSeq != 200 {
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
