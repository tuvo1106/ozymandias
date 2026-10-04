package logstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func entries(n int, startTs int64) []rawEntry {
	out := make([]rawEntry, n)
	for i := range out {
		out[i] = rawEntry{Ts: startTs + int64(i)*7, Body: []byte(fmt.Sprintf(`{"message":"line %d"}`, i))}
	}
	return out
}

func mustEncode(t testing.TB, es []rawEntry, seq uint64) (BlockMeta, []byte) {
	t.Helper()
	m, comp, err := encodeBlock(es, seq)
	if err != nil {
		t.Fatal(err)
	}
	return m, comp
}

func TestBlock_RoundTrip(t *testing.T) {
	for name, es := range map[string][]rawEntry{
		"one":          entries(1, 1_790_000_000_000),
		"many":         entries(500, 1_790_000_000_000),
		"empty bodies": {{Ts: 5, Body: nil}, {Ts: 5, Body: []byte{}}, {Ts: 6, Body: []byte("x")}},
		"out of order": {{Ts: 100, Body: []byte("a")}, {Ts: 50, Body: []byte("b")}, {Ts: 200, Body: []byte("c")}},
		"negative":     {{Ts: -5, Body: []byte("a")}, {Ts: 0, Body: []byte("b")}},
		"extremes":     {{Ts: 1 << 62, Body: []byte("a")}, {Ts: 0, Body: []byte("b")}},
	} {
		m, comp := mustEncode(t, es, 42)
		got, err := decodeBlock(m, comp)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(got) != len(es) {
			t.Errorf("%s: %d entries back, want %d", name, len(got), len(es))
			continue
		}
		for i := range es {
			if got[i].Ts != es[i].Ts || !bytes.Equal(got[i].Body, es[i].Body) {
				t.Errorf("%s entry %d: got %v, want %v", name, i, got[i], es[i])
			}
		}
		if m.N != uint32(len(es)) || m.LastSeq != 42 {
			t.Errorf("%s: meta %+v", name, m)
		}
	}
	if _, _, err := encodeBlock(nil, 0); err == nil {
		t.Error("an empty block was encoded")
	}
}

func TestBlock_MinMaxFollowTheEntriesNotTheirOrder(t *testing.T) {
	m, _ := mustEncode(t, []rawEntry{{Ts: 100}, {Ts: 50}, {Ts: 200}}, 1)
	if m.MinTs != 50 || m.MaxTs != 200 {
		t.Errorf("min/max = %d/%d, want 50/200", m.MinTs, m.MaxTs)
	}
}

func TestBlock_RejectsDamage(t *testing.T) {
	m, comp := mustEncode(t, entries(50, 1000), 1)
	for name, fn := range map[string]func() (BlockMeta, []byte){
		"flipped bit":    func() (BlockMeta, []byte) { c := bytes.Clone(comp); c[len(c)/2] ^= 1; return m, c },
		"short":          func() (BlockMeta, []byte) { return m, comp[:len(comp)-1] },
		"wrong rawLen":   func() (BlockMeta, []byte) { x := m; x.RawLen++; return x, comp },
		"wrong count":    func() (BlockMeta, []byte) { x := m; x.N++; return x, comp },
		"fewer entries":  func() (BlockMeta, []byte) { x := m; x.N--; return x, comp },
		"huge rawLen":    func() (BlockMeta, []byte) { x := m; x.RawLen = maxBlockRaw + 1; return x, comp },
		"huge count":     func() (BlockMeta, []byte) { x := m; x.N = maxBlockCount + 1; return x, comp },
		"wrong checksum": func() (BlockMeta, []byte) { x := m; x.CRC++; return x, comp },
	} {
		bm, c := fn()
		if _, err := decodeBlock(bm, c); !errors.Is(err, ErrCorruptChunk) {
			t.Errorf("%s: err = %v, want ErrCorruptChunk", name, err)
		}
	}
}

// A block that claims a tiny rawLen but inflates enormously must be refused
// by the decoder's memory cap, not allocated.
func TestBlock_DecompressionBombIsRefused(t *testing.T) {
	enc, _ := codec()
	bomb := enc.EncodeAll(make([]byte, 64<<20), nil) // 64 MiB of zeros compresses to a few bytes
	m := BlockMeta{N: 1, RawLen: 10, CompLen: uint32(len(bomb)), CRC: crc(bomb)}
	if _, err := decodeBlock(m, bomb); !errors.Is(err, ErrCorruptChunk) {
		t.Errorf("err = %v, want ErrCorruptChunk", err)
	}
}

func crc(b []byte) uint32 { return crcOf(b) }

// writeChunk writes blocks to a new chunk file and returns its path.
func writeChunk(t testing.TB, dir string, blocks [][]rawEntry, seal bool) string {
	t.Helper()
	path := filepath.Join(dir, "c.chunk")
	w, err := openChunk(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, es := range blocks {
		m, comp := mustEncode(t, es, uint64(i+1)*100)
		if _, err := w.appendBlock(m, comp); err != nil {
			t.Fatal(err)
		}
	}
	if seal {
		if err := w.seal(); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func readAll(t testing.TB, path string) ([]byte, ChunkIndex) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := readIndex(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	return data, ix
}

func TestChunk_WriteThenReadWithAndWithoutFooter(t *testing.T) {
	blocks := [][]rawEntry{entries(10, 1000), entries(20, 5000), entries(5, 9000)}
	for _, seal := range []bool{true, false} {
		path := writeChunk(t, t.TempDir(), blocks, seal)
		data, ix := readAll(t, path)
		if ix.Sealed != seal {
			t.Errorf("seal=%v: Sealed = %v", seal, ix.Sealed)
		}
		if len(ix.Blocks) != 3 {
			t.Fatalf("seal=%v: %d blocks", seal, len(ix.Blocks))
		}
		for i, m := range ix.Blocks {
			got, err := readBlock(bytes.NewReader(data), m)
			if err != nil || len(got) != len(blocks[i]) {
				t.Errorf("seal=%v block %d: %d entries, err %v", seal, i, len(got), err)
			}
			if m.LastSeq != uint64(i+1)*100 {
				t.Errorf("seal=%v block %d: LastSeq %d", seal, i, m.LastSeq)
			}
		}
		if seal && ix.ValidEnd+int64(3*indexEntrySz+trailerSize) != int64(len(data)) {
			t.Errorf("footer is not where the index says: ValidEnd %d, file %d", ix.ValidEnd, len(data))
		}
	}
}

// A crash can cut the file anywhere. Cut it at every byte: the reader must
// never panic, must serve exactly the blocks that are whole, and a writer
// reopening it must be able to carry on.
func TestChunk_TornAtEveryOffsetServesTheValidPrefix(t *testing.T) {
	blocks := [][]rawEntry{entries(8, 1000), entries(8, 2000), entries(8, 3000)}
	path := writeChunk(t, t.TempDir(), blocks, false)
	full, fullIx := readAll(t, path)
	for cut := 0; cut <= len(full); cut++ {
		data := full[:cut]
		ix, err := readIndex(bytes.NewReader(data), int64(cut))
		if cut < chunkHdrSize {
			if err == nil {
				t.Fatalf("cut %d: a file shorter than the header was accepted", cut)
			}
			continue
		}
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		whole := 0
		for _, m := range fullIx.Blocks {
			if m.end() <= int64(cut) {
				whole++
			}
		}
		if len(ix.Blocks) != whole {
			t.Fatalf("cut %d: %d blocks served, %d are whole", cut, len(ix.Blocks), whole)
		}
		for _, m := range ix.Blocks {
			if _, err := readBlock(bytes.NewReader(data), m); err != nil {
				t.Fatalf("cut %d: a block the index vouched for does not read: %v", cut, err)
			}
		}
	}
}

func TestChunk_CorruptFooterFallsBackToTheWalk(t *testing.T) {
	path := writeChunk(t, t.TempDir(), [][]rawEntry{entries(8, 1000), entries(8, 2000)}, true)
	data, _ := readAll(t, path)
	for name, mut := range map[string]func([]byte){
		"index byte":  func(b []byte) { b[len(b)-trailerSize-5] ^= 0xff },
		"trailer crc": func(b []byte) { b[len(b)-8] ^= 0xff },
		"trailer off": func(b []byte) { binary.BigEndian.PutUint64(b[len(b)-trailerSize+4:], 3) },
		"trailer cnt": func(b []byte) { binary.BigEndian.PutUint32(b[len(b)-trailerSize:], 99) },
	} {
		b := bytes.Clone(data)
		mut(b)
		ix, err := readIndex(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if ix.Sealed || len(ix.Blocks) != 2 {
			t.Errorf("%s: sealed=%v blocks=%d, want a walk that finds both blocks", name, ix.Sealed, len(ix.Blocks))
		}
	}
}

func TestChunk_BitRotInABlockEndsTheValidPrefixThere(t *testing.T) {
	path := writeChunk(t, t.TempDir(), [][]rawEntry{entries(30, 1000), entries(30, 2000), entries(30, 3000)}, false)
	data, ix := readAll(t, path)
	b := bytes.Clone(data)
	b[ix.Blocks[1].Offset+blockHdrSize+3] ^= 0xff // inside block 1's payload
	got, err := readIndex(bytes.NewReader(b), int64(len(b)))
	if err != nil || len(got.Blocks) != 1 {
		t.Fatalf("blocks = %d, err %v; want the prefix before the damaged block", len(got.Blocks), err)
	}
}

// Reopening a sealed chunk cuts the footer off and appends where the last
// block ended; sealing again writes a footer that covers all of it.
func TestChunk_ReopenAppendsAfterTheLastBlock(t *testing.T) {
	dir := t.TempDir()
	path := writeChunk(t, dir, [][]rawEntry{entries(5, 1000)}, true)
	w, err := openChunk(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.blocks) != 1 {
		t.Fatalf("reopened with %d blocks", len(w.blocks))
	}
	m, comp := mustEncode(t, entries(5, 2000), 9)
	if _, err := w.appendBlock(m, comp); err != nil {
		t.Fatal(err)
	}
	if err := w.seal(); err != nil {
		t.Fatal(err)
	}
	_ = w.close()
	data, ix := readAll(t, path)
	if !ix.Sealed || len(ix.Blocks) != 2 {
		t.Fatalf("sealed=%v blocks=%d", ix.Sealed, len(ix.Blocks))
	}
	for i, bm := range ix.Blocks {
		if es, err := readBlock(bytes.NewReader(data), bm); err != nil || len(es) != 5 {
			t.Errorf("block %d: %d entries, %v", i, len(es), err)
		}
	}
}

func TestChunk_ReopenAfterATornTailCutsItOff(t *testing.T) {
	path := writeChunk(t, t.TempDir(), [][]rawEntry{entries(5, 1000), entries(5, 2000)}, false)
	data, ix := readAll(t, path)
	if err := os.WriteFile(path, data[:ix.Blocks[1].end()-3], 0o644); err != nil { // tear the second block
		t.Fatal(err)
	}
	w, err := openChunk(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.close() }()
	if len(w.blocks) != 1 || w.end != ix.Blocks[0].end() {
		t.Fatalf("resumed at %d with %d blocks, want the end of block 0 (%d)", w.end, len(w.blocks), ix.Blocks[0].end())
	}
	if st, _ := os.Stat(path); st.Size() != w.end {
		t.Errorf("file is %d bytes; the torn tail was not cut to %d", st.Size(), w.end)
	}
}

func TestChunk_RejectsAFileThatIsNotOne(t *testing.T) {
	for name, b := range map[string][]byte{
		"empty":         nil,
		"short":         []byte("OZY"),
		"wrong magic":   append([]byte("NOPE\x00\x01\x00\x00"), make([]byte, 20)...),
		"wrong version": append([]byte("OZYC\x00\x09\x00\x00"), make([]byte, 20)...),
	} {
		if _, err := readIndex(bytes.NewReader(b), int64(len(b))); !errors.Is(err, ErrCorruptChunk) {
			t.Errorf("%s: err = %v, want ErrCorruptChunk", name, err)
		}
	}
}

// L4: whatever bytes arrive, reading a chunk neither panics nor trusts a
// length that does not fit, and every block the index offers either decodes
// or fails cleanly.
func FuzzReadChunk(f *testing.F) {
	dir := f.TempDir()
	sealed, _ := os.ReadFile(writeChunk(f, dir, [][]rawEntry{entries(6, 1000), entries(3, 9000)}, true))
	f.Add(sealed)
	f.Add(sealed[:len(sealed)/2])
	f.Add([]byte("OZYC\x00\x01\x00\x00"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		ix, err := readIndex(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		if ix.ValidEnd > int64(len(data)) {
			t.Fatalf("ValidEnd %d is past the end of a %d-byte file", ix.ValidEnd, len(data))
		}
		prev := int64(chunkHdrSize)
		for _, m := range ix.Blocks {
			if m.Offset != prev || m.end() > ix.ValidEnd {
				t.Fatalf("blocks do not tile the valid region: %+v after %d (valid end %d)", m, prev, ix.ValidEnd)
			}
			prev = m.end()
			if es, err := readBlock(bytes.NewReader(data), m); err == nil && uint32(len(es)) != m.N {
				t.Fatalf("block decoded %d entries, header says %d", len(es), m.N)
			}
		}
	})
}

// --- crafted inputs: structurally wrong, checksums right ---
//
// Flipping bytes is caught by a checksum before any structural check runs, so
// it cannot show whether those checks exist. These files have valid checksums
// and wrong structure, which is what a writer bug produces.

// chunkParts writes a two-block unsealed chunk and returns its bytes with
// each block's meta, so a test can assemble a footer by hand.
func chunkParts(t *testing.T) ([]byte, []BlockMeta) {
	t.Helper()
	data, ix := readAll(t, writeChunk(t, t.TempDir(), [][]rawEntry{entries(6, 1000), entries(6, 2000)}, false))
	return data, ix.Blocks
}

func withTrailer(data []byte, index []byte, count uint32, indexOffset int64, extraBeforeTrailer int) []byte {
	out := append(bytes.Clone(data), index...)
	out = append(out, make([]byte, extraBeforeTrailer)...)
	var tr [trailerSize]byte
	binary.BigEndian.PutUint32(tr[0:], count)
	binary.BigEndian.PutUint64(tr[4:], uint64(indexOffset))
	binary.BigEndian.PutUint32(tr[12:], crcOf(index))
	copy(tr[16:], footerMagic)
	return append(out, tr[:]...)
}

func indexBytes(blocks []BlockMeta) []byte {
	f := encodeFooter(blocks, 0)
	return f[:len(f)-trailerSize]
}

func TestChunk_AFooterThatDoesNotTileTheFileIsIgnored(t *testing.T) {
	data, blocks := chunkParts(t)
	end := blocks[1].end()
	shifted := append([]BlockMeta(nil), blocks...)
	shifted[1].Offset++ // does not start where block 0 ended
	short := blocks[:1] // vouches for less than the file holds
	firstEarly := append([]BlockMeta(nil), blocks...)
	firstEarly[0].Offset-- // block 0 claims to start inside the header; block 1 is right

	for name, file := range map[string][]byte{
		"offsets shifted":          withTrailer(data, indexBytes(shifted), 2, end, 0),
		"first block misplaced":    withTrailer(data, indexBytes(firstEarly), 2, end, 0),
		"index covers one block":   withTrailer(data, indexBytes(short), 1, end, 0),
		"bytes after the index":    withTrailer(data, indexBytes(blocks), 2, end, 5),
		"index offset before data": withTrailer(data, indexBytes(blocks), 2, blocks[0].Offset, 0),
	} {
		ix, err := readIndex(bytes.NewReader(file), int64(len(file)))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if ix.Sealed || len(ix.Blocks) != 2 {
			t.Errorf("%s: sealed=%v blocks=%d; a footer that disagrees with the file must be ignored and the blocks found by walking", name, ix.Sealed, len(ix.Blocks))
		}
	}
	// And the well-formed footer over the same bytes is accepted, so the cases
	// above fail for the right reason.
	good := withTrailer(data, indexBytes(blocks), 2, end, 0)
	if ix, _ := readIndex(bytes.NewReader(good), int64(len(good))); !ix.Sealed {
		t.Error("a well-formed footer was not trusted")
	}
}

func TestChunk_ImplausibleBlockHeadersEndTheWalk(t *testing.T) {
	good, blocks := chunkParts(t)
	_, comp := mustEncode(t, entries(3, 7000), 1)
	for name, mutate := range map[string]func(*BlockMeta){
		"no entries":     func(m *BlockMeta) { m.N = 0 },
		"min above max":  func(m *BlockMeta) { m.MinTs, m.MaxTs = 10, 5 },
		"rawLen too big": func(m *BlockMeta) { m.RawLen = maxBlockRaw + 1 },
	} {
		m, _ := mustEncode(t, entries(3, 7000), 1)
		mutate(&m)
		bad := make([]byte, blockHdrSize)
		putBlockHeader(bad, m) // checksum field is right for comp, so only the sanity checks can object
		file := append(append(bytes.Clone(good), bad...), comp...)
		ix, err := readIndex(bytes.NewReader(file), int64(len(file)))
		if err != nil || len(ix.Blocks) != len(blocks) {
			t.Errorf("%s: %d blocks, err %v; the walk should stop before the implausible block", name, len(ix.Blocks), err)
		}
	}
}

func TestBlock_AnEntryLengthPastTheBlockIsAnErrorNotAPanic(t *testing.T) {
	// ts delta 0, then a length of 1000 with 3 bytes following.
	raw := append([]byte{0}, binary.AppendUvarint(nil, 1000)...)
	raw = append(raw, 'a', 'b', 'c')
	enc, _ := codec()
	comp := enc.EncodeAll(raw, nil)
	m := BlockMeta{N: 1, RawLen: uint32(len(raw)), CompLen: uint32(len(comp)), CRC: crcOf(comp)}
	if _, err := decodeBlock(m, comp); !errors.Is(err, ErrCorruptChunk) {
		t.Errorf("err = %v, want ErrCorruptChunk", err)
	}
}

func TestChunk_ReadBlockChecksTheDiskHeaderAgainstTheIndex(t *testing.T) {
	data, blocks := chunkParts(t)
	m := blocks[0]
	m.N++ // the index claims a different entry count than the block's own header
	if _, err := readBlock(bytes.NewReader(data), m); !errors.Is(err, ErrCorruptChunk) {
		t.Errorf("err = %v, want ErrCorruptChunk", err)
	}
}

// A header that lies about its size must be refused before anything is
// allocated for it: the guard exists to stop a corrupt block from costing
// gigabytes, which only an allocation measurement can show.
func TestBlock_ALyingHeaderAllocatesNothing(t *testing.T) {
	m, comp := mustEncode(t, entries(5, 1000), 1)
	m.RawLen, m.N = 1<<30, maxBlockCount+1
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := decodeBlock(m, comp)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrCorruptChunk) {
		t.Fatalf("err = %v", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
		t.Errorf("decoding a lying header allocated %d MiB", grew>>20)
	}
}

// The decoder's memory cap is what stops a block from inflating without
// bound before its length can be compared with the header.
func TestCodec_DecoderRefusesToInflatePastTheCap(t *testing.T) {
	enc, dec := codec()
	bomb := enc.EncodeAll(make([]byte, maxBlockRaw+(4<<20)), nil)
	if len(bomb) > 1<<16 {
		t.Fatalf("test setup: the bomb is %d bytes, expected a few", len(bomb))
	}
	if out, err := dec.DecodeAll(bomb, nil); err == nil {
		t.Errorf("decoded %d bytes past the %d-byte cap", len(out), maxBlockRaw)
	}
}
