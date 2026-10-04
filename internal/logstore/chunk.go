package logstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// The chunk file (docs/formats/log-chunk.md): a header, then blocks, then —
// when the chunk has been closed cleanly — a footer index.
//
//	header   "OZYC" | version u16 | reserved u16                       8 bytes
//	block    minTs i64 | maxTs i64 | n u32 | rawLen u32 | compLen u32
//	         | crc32c u32 (of the compressed bytes) | lastSeq u64       40 bytes
//	         | bloomLen u32 (v2 only)                                    +4 bytes
//	         | zstd(entries)                                             compLen bytes
//	         | bloom filter (v2 only)                                    bloomLen bytes
//	footer   one 44-byte (v2: 48) index entry per block, then
//	         count u32 | indexOffset u64 | crc32c u32 | "OZYF"          20 bytes
//
// Version 1 chunks have no filters and stay readable. A file keeps the version
// it was created with, even when a newer binary appends to it, so one file is
// never a mix of block layouts; the new version starts with the next day's chunk.
//
// Everything is big-endian. A block is self-describing, so the footer is an
// optimization and never the only copy of anything: a chunk that was killed
// mid-write has no footer, and the reader finds its blocks by walking the
// headers and stops at the first one that does not check out. That is what
// "a torn tail serves its valid prefix" means in practice.
const (
	chunkMagic    = "OZYC"
	footerMagic   = "OZYF"
	chunkVersion  = 2 // the version new chunks are written in
	chunkHdrSize  = 8
	blockHdrSize  = 40 // v1; v2 adds bloomLen (see hdrSize)
	indexEntrySz  = 44 // v1; v2 adds bloomLen (see entrySize)
	trailerSize   = 20
	maxBlockRaw   = 16 << 20 // a block is sealed at 256 KiB; this only bounds a corrupt rawLen
	maxBlockComp  = 16 << 20
	maxBlockCount = 1 << 22
	maxBloomLen   = 1 << 20 // a filter is at most maxBloomBytes; this bounds a corrupt bloomLen
)

// newChunkVersion is the version a new chunk file is created in. It is a
// variable only so tests can write v1 chunks and prove they still read; nothing
// else assigns it.
var newChunkVersion uint16 = chunkVersion

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrCorruptChunk is wrapped by every error that means the bytes of a chunk
// are not a valid chunk (as opposed to an I/O failure).
var ErrCorruptChunk = errors.New("logstore: corrupt chunk")

// BlockMeta is a block's header plus where it lives in the file: what the
// footer index holds, and what a read needs to decide whether to decode it.
type BlockMeta struct {
	Offset  int64  // of the block header
	MinTs   int64  // unix ms
	MaxTs   int64  // unix ms
	N       uint32 // entries
	RawLen  uint32 // uncompressed bytes
	CompLen uint32 // compressed bytes
	CRC     uint32 // crc32c of the compressed bytes
	LastSeq uint64 // highest WAL sequence number of any entry in the block
	// BloomLen is the bytes of bloom filter after the compressed data (v2; 0 for
	// a block with none).
	BloomLen uint32
	// ver is the chunk version the block was read from or written in. It is not
	// stored per block: it is the file's, and it decides the header layout.
	ver uint16
}

// hdrSize is the block header's length in a chunk of version v.
func hdrSize(v uint16) int {
	if v >= 2 {
		return blockHdrSize + 4
	}
	return blockHdrSize
}

// entrySize is the footer index entry's length in a chunk of version v.
func entrySize(v uint16) int {
	if v >= 2 {
		return indexEntrySz + 4
	}
	return indexEntrySz
}

func (m BlockMeta) hdr() int64 { return int64(hdrSize(m.ver)) }

// end is the offset one past the block's last byte.
func (m BlockMeta) end() int64 {
	return m.Offset + m.hdr() + int64(m.CompLen) + int64(m.BloomLen)
}

// bloomOffset is where the block's filter starts.
func (m BlockMeta) bloomOffset() int64 { return m.Offset + m.hdr() + int64(m.CompLen) }

// rawEntry is one log as the chunk layer sees it: a timestamp, a sequence
// number and an opaque body. The store above decides what the body is (the
// log's JSON); this layer only orders, compresses and checks.
//
// Seq is the WAL sequence number the store gave the entry. It is unique and
// survives sealing and restarts, which makes (Ts, Seq) a total order over
// every log ever stored: what a pagination cursor needs, and what recovery
// uses to tell a sealed entry from one still only in the WAL.
type rawEntry struct {
	Ts   int64
	Seq  uint64
	Body []byte
}

var (
	encoderOnce sync.Once
	encoder     *zstd.Encoder
	decoder     *zstd.Decoder
)

// codec returns the shared zstd encoder and decoder. Both are safe for
// concurrent EncodeAll/DecodeAll, and building them is the expensive part, so
// they are made once. The decoder's memory cap is the decompression-bomb
// guard: a block that claims a small rawLen cannot inflate past it.
func codec() (*zstd.Encoder, *zstd.Decoder) {
	encoderOnce.Do(func() {
		var err error
		encoder, err = zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			panic(err) // static options; cannot fail
		}
		decoder, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(maxBlockRaw))
		if err != nil {
			panic(err)
		}
	})
	return encoder, decoder
}

// encodeBlock packs entries into a block: delta-encoded timestamps and
// sequence numbers, and length-prefixed bodies, then zstd. Both deltas are
// zigzag from the previous entry (timestamps from MinTs, sequences from 0), so
// a run of nearby values costs a byte or two each, and out-of-order input is
// representable (the store sorts before sealing, but this layer does not
// depend on it). The block's LastSeq is the highest Seq among its entries.
func encodeBlock(entries []rawEntry) (BlockMeta, []byte, error) {
	if len(entries) == 0 {
		return BlockMeta{}, nil, errors.New("logstore: a block needs at least one entry")
	}
	minTs, maxTs, lastSeq := entries[0].Ts, entries[0].Ts, entries[0].Seq
	for _, e := range entries {
		minTs, maxTs, lastSeq = min(minTs, e.Ts), max(maxTs, e.Ts), max(lastSeq, e.Seq)
	}
	var raw []byte
	var tmp [binary.MaxVarintLen64]byte
	prev, prevSeq := minTs, uint64(0)
	for _, e := range entries {
		raw = append(raw, tmp[:binary.PutVarint(tmp[:], e.Ts-prev)]...)
		raw = append(raw, tmp[:binary.PutVarint(tmp[:], int64(e.Seq-prevSeq))]...)
		raw = append(raw, tmp[:binary.PutUvarint(tmp[:], uint64(len(e.Body)))]...)
		raw = append(raw, e.Body...)
		prev, prevSeq = e.Ts, e.Seq
	}
	if len(raw) > maxBlockRaw {
		return BlockMeta{}, nil, fmt.Errorf("logstore: block of %d bytes exceeds the %d limit", len(raw), maxBlockRaw)
	}
	enc, _ := codec()
	comp := enc.EncodeAll(raw, nil)
	return BlockMeta{
		MinTs: minTs, MaxTs: maxTs, N: uint32(len(entries)),
		RawLen: uint32(len(raw)), CompLen: uint32(len(comp)),
		CRC: crc32.Checksum(comp, castagnoli), LastSeq: lastSeq,
	}, comp, nil
}

// decodeBlock is the inverse of encodeBlock. It checks the checksum and every
// length it reads, because it is what runs over bytes that may have rotted:
// a block that does not verify is an error, never a panic and never garbage.
func decodeBlock(m BlockMeta, comp []byte) ([]rawEntry, error) {
	if uint32(len(comp)) != m.CompLen {
		return nil, fmt.Errorf("%w: block has %d bytes, header says %d", ErrCorruptChunk, len(comp), m.CompLen)
	}
	if crc32.Checksum(comp, castagnoli) != m.CRC {
		return nil, fmt.Errorf("%w: block checksum mismatch", ErrCorruptChunk)
	}
	if m.RawLen > maxBlockRaw || m.N > maxBlockCount {
		return nil, fmt.Errorf("%w: implausible block header (rawLen %d, n %d)", ErrCorruptChunk, m.RawLen, m.N)
	}
	_, dec := codec()
	raw, err := dec.DecodeAll(comp, make([]byte, 0, m.RawLen))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptChunk, err)
	}
	if uint32(len(raw)) != m.RawLen {
		return nil, fmt.Errorf("%w: block inflates to %d bytes, header says %d", ErrCorruptChunk, len(raw), m.RawLen)
	}
	out := make([]rawEntry, 0, m.N)
	prev, prevSeq := m.MinTs, uint64(0)
	for i := uint32(0); i < m.N; i++ {
		d, k := binary.Varint(raw)
		if k <= 0 {
			return nil, fmt.Errorf("%w: entry %d: bad timestamp", ErrCorruptChunk, i)
		}
		raw = raw[k:]
		ds, k := binary.Varint(raw)
		if k <= 0 {
			return nil, fmt.Errorf("%w: entry %d: bad sequence number", ErrCorruptChunk, i)
		}
		raw = raw[k:]
		l, k := binary.Uvarint(raw)
		if k <= 0 || l > uint64(len(raw)-k) {
			return nil, fmt.Errorf("%w: entry %d: bad length", ErrCorruptChunk, i)
		}
		ts, seq := prev+d, prevSeq+uint64(ds)
		out = append(out, rawEntry{Ts: ts, Seq: seq, Body: raw[k : k+int(l)]})
		raw = raw[k+int(l):]
		prev, prevSeq = ts, seq
	}
	if len(raw) != 0 {
		return nil, fmt.Errorf("%w: %d bytes after the last entry", ErrCorruptChunk, len(raw))
	}
	return out, nil
}

func putBlockHeader(b []byte, m BlockMeta) {
	if m.ver >= 2 {
		binary.BigEndian.PutUint32(b[40:], m.BloomLen)
	}
	binary.BigEndian.PutUint64(b[0:], uint64(m.MinTs))
	binary.BigEndian.PutUint64(b[8:], uint64(m.MaxTs))
	binary.BigEndian.PutUint32(b[16:], m.N)
	binary.BigEndian.PutUint32(b[20:], m.RawLen)
	binary.BigEndian.PutUint32(b[24:], m.CompLen)
	binary.BigEndian.PutUint32(b[28:], m.CRC)
	binary.BigEndian.PutUint64(b[32:], m.LastSeq)
}

func getBlockHeader(b []byte, offset int64, ver uint16) BlockMeta {
	var bloomLen uint32
	if ver >= 2 {
		bloomLen = binary.BigEndian.Uint32(b[40:])
	}
	return BlockMeta{
		BloomLen: bloomLen, ver: ver,
		Offset:  offset,
		MinTs:   int64(binary.BigEndian.Uint64(b[0:])),
		MaxTs:   int64(binary.BigEndian.Uint64(b[8:])),
		N:       binary.BigEndian.Uint32(b[16:]),
		RawLen:  binary.BigEndian.Uint32(b[20:]),
		CompLen: binary.BigEndian.Uint32(b[24:]),
		CRC:     binary.BigEndian.Uint32(b[28:]),
		LastSeq: binary.BigEndian.Uint64(b[32:]),
	}
}

// plausible reports whether a header read off disk could be a real block
// that fits in a file of the given size. It is what stops a scan from
// trusting a length that points past the end, or a count that would make a
// reader allocate gigabytes, before a checksum has had a chance to say no.
func (m BlockMeta) plausible(fileSize int64) bool {
	return m.N > 0 && m.N <= maxBlockCount && m.RawLen <= maxBlockRaw && m.CompLen <= maxBlockComp &&
		m.BloomLen <= maxBloomLen && m.MinTs <= m.MaxTs && m.end() <= fileSize
}

// ChunkIndex is what a reader learns about a chunk file: its blocks, where
// the valid data ends, and whether a footer vouched for them.
type ChunkIndex struct {
	Blocks []BlockMeta
	// ValidEnd is the offset one past the last good block. A writer resuming
	// the file truncates here, which cuts off a footer or a torn tail alike.
	ValidEnd int64
	// Sealed is true if the footer was present and intact.
	Sealed bool
	// Version is the chunk file's format version.
	Version uint16
}

// readIndex learns a chunk's blocks from r, which holds size bytes. It
// prefers the footer and falls back to walking block headers; the walk also
// verifies each block's checksum, so a torn or rotted tail ends the valid
// prefix instead of being served.
func readIndex(r io.ReaderAt, size int64) (ChunkIndex, error) {
	var hdr [chunkHdrSize]byte
	if size < chunkHdrSize {
		return ChunkIndex{}, fmt.Errorf("%w: %d bytes is shorter than a header", ErrCorruptChunk, size)
	}
	if _, err := r.ReadAt(hdr[:], 0); err != nil {
		return ChunkIndex{}, err
	}
	if string(hdr[:4]) != chunkMagic {
		return ChunkIndex{}, fmt.Errorf("%w: bad magic %q", ErrCorruptChunk, hdr[:4])
	}
	ver := binary.BigEndian.Uint16(hdr[4:])
	if ver < 1 || ver > chunkVersion {
		return ChunkIndex{}, fmt.Errorf("%w: unsupported version %d", ErrCorruptChunk, ver)
	}
	if ix, ok := readFooter(r, size, ver); ok {
		return ix, nil
	}
	return walkBlocks(r, size, ver)
}

func readFooter(r io.ReaderAt, size int64, ver uint16) (ChunkIndex, bool) {
	esz := int64(entrySize(ver))
	if size < chunkHdrSize+trailerSize {
		return ChunkIndex{}, false
	}
	var t [trailerSize]byte
	if _, err := r.ReadAt(t[:], size-trailerSize); err != nil || string(t[16:]) != footerMagic {
		return ChunkIndex{}, false
	}
	count := binary.BigEndian.Uint32(t[0:])
	off := int64(binary.BigEndian.Uint64(t[4:]))
	crc := binary.BigEndian.Uint32(t[12:])
	if count > maxBlockCount || off+int64(count)*esz != size-trailerSize {
		return ChunkIndex{}, false
	}
	buf := make([]byte, int64(count)*esz)
	if _, err := r.ReadAt(buf, off); err != nil || crc32.Checksum(buf, castagnoli) != crc {
		return ChunkIndex{}, false
	}
	ix := ChunkIndex{Blocks: make([]BlockMeta, count), ValidEnd: off, Sealed: true, Version: ver}
	prevEnd := int64(chunkHdrSize)
	for i := range ix.Blocks {
		e := buf[int64(i)*esz:]
		m := BlockMeta{
			ver:     ver,
			Offset:  int64(binary.BigEndian.Uint64(e[0:])),
			MinTs:   int64(binary.BigEndian.Uint64(e[8:])),
			MaxTs:   int64(binary.BigEndian.Uint64(e[16:])),
			N:       binary.BigEndian.Uint32(e[24:]),
			RawLen:  binary.BigEndian.Uint32(e[28:]),
			CompLen: binary.BigEndian.Uint32(e[32:]),
			LastSeq: binary.BigEndian.Uint64(e[36:]),
		}
		if ver >= 2 {
			m.BloomLen = binary.BigEndian.Uint32(e[44:])
		}
		// The index does not carry the block's crc (the block header does,
		// and decoding checks it); it must still tile the file.
		if m.Offset != prevEnd || !m.plausible(off) {
			return ChunkIndex{}, false
		}
		prevEnd = m.end()
		ix.Blocks[i] = m
	}
	if prevEnd != off {
		return ChunkIndex{}, false
	}
	return ix, true
}

func walkBlocks(r io.ReaderAt, size int64, ver uint16) (ChunkIndex, error) {
	ix := ChunkIndex{ValidEnd: chunkHdrSize, Version: ver}
	hsz := int64(hdrSize(ver))
	hdr := make([]byte, hsz)
	for ix.ValidEnd+hsz <= size {
		if _, err := r.ReadAt(hdr, ix.ValidEnd); err != nil {
			return ix, err
		}
		m := getBlockHeader(hdr, ix.ValidEnd, ver)
		if !m.plausible(size) {
			break
		}
		comp := make([]byte, m.CompLen)
		if _, err := r.ReadAt(comp, ix.ValidEnd+hsz); err != nil {
			return ix, err
		}
		if crc32.Checksum(comp, castagnoli) != m.CRC {
			break
		}
		ix.Blocks = append(ix.Blocks, m)
		ix.ValidEnd = m.end()
	}
	return ix, nil
}

// readBlock reads and decodes one block of a chunk file.
func readBlock(r io.ReaderAt, m BlockMeta) ([]rawEntry, error) {
	hsz := int(m.hdr())
	buf := make([]byte, hsz+int(m.CompLen))
	if _, err := r.ReadAt(buf, m.Offset); err != nil {
		return nil, fmt.Errorf("logstore: reading block at %d: %w", m.Offset, err)
	}
	disk := getBlockHeader(buf, m.Offset, m.ver)
	if disk.N != m.N || disk.RawLen != m.RawLen || disk.CompLen != m.CompLen || disk.MinTs != m.MinTs || disk.MaxTs != m.MaxTs ||
		disk.BloomLen != m.BloomLen {
		return nil, fmt.Errorf("%w: block at %d does not match its index entry", ErrCorruptChunk, m.Offset)
	}
	return decodeBlock(disk, buf[hsz:])
}

// readBloom reads and verifies a block's filter. ok is false when the block has
// none, when it cannot be read, or when it fails its checksum: in every case the
// caller reads the block instead, which is slower and always correct.
func readBloom(r io.ReaderAt, m BlockMeta) (bloomView, bool) {
	if m.BloomLen == 0 {
		return bloomView{}, false
	}
	blob := make([]byte, m.BloomLen)
	if _, err := r.ReadAt(blob, m.bloomOffset()); err != nil {
		return bloomView{}, false
	}
	return parseBloom(blob)
}

// encodeFooter builds the footer index for blocks, which must tile the file
// from the header to indexOffset.
func encodeFooter(blocks []BlockMeta, indexOffset int64, ver uint16) []byte {
	esz := entrySize(ver)
	buf := make([]byte, len(blocks)*esz, len(blocks)*esz+trailerSize)
	for i, m := range blocks {
		e := buf[i*esz:]
		if ver >= 2 {
			binary.BigEndian.PutUint32(e[44:], m.BloomLen)
		}
		binary.BigEndian.PutUint64(e[0:], uint64(m.Offset))
		binary.BigEndian.PutUint64(e[8:], uint64(m.MinTs))
		binary.BigEndian.PutUint64(e[16:], uint64(m.MaxTs))
		binary.BigEndian.PutUint32(e[24:], m.N)
		binary.BigEndian.PutUint32(e[28:], m.RawLen)
		binary.BigEndian.PutUint32(e[32:], m.CompLen)
		binary.BigEndian.PutUint64(e[36:], m.LastSeq)
	}
	var t [trailerSize]byte
	binary.BigEndian.PutUint32(t[0:], uint32(len(blocks)))
	binary.BigEndian.PutUint64(t[4:], uint64(indexOffset))
	binary.BigEndian.PutUint32(t[12:], crc32.Checksum(buf, castagnoli))
	copy(t[16:], footerMagic)
	return append(buf, t[:]...)
}

// chunkWriter appends blocks to one chunk file. It is not safe for concurrent
// use; the store serializes writers per chunk.
type chunkWriter struct {
	f      *os.File
	path   string
	blocks []BlockMeta
	end    int64  // offset where the next block goes
	ver    uint16 // the file's format version: new files are chunkVersion, old ones keep theirs
	// noSync skips fsync (Options.NoSync: tests and benchmarks only).
	noSync bool
}

// openChunk opens the chunk at path for appending, creating it if it does not
// exist. An existing file is read first: whatever follows its last good block
// (a footer from a clean close, or a torn tail from a crash) is cut off, so
// the file always ends where the next block begins and the footer is written
// again on close.
func openChunk(path string, noSync bool) (*chunkWriter, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("logstore: opening chunk: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	w := &chunkWriter{f: f, path: path, noSync: noSync}
	if st.Size() == 0 {
		var hdr [chunkHdrSize]byte
		copy(hdr[:], chunkMagic)
		binary.BigEndian.PutUint16(hdr[4:], newChunkVersion)
		w.ver = newChunkVersion
		if _, err := f.WriteAt(hdr[:], 0); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("logstore: writing chunk header: %w", err)
		}
		w.end = chunkHdrSize
		return w, nil
	}
	ix, err := readIndex(f, st.Size())
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("logstore: reopening %s: %w", path, err)
	}
	if ix.ValidEnd < st.Size() {
		if err := f.Truncate(ix.ValidEnd); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("logstore: cutting %s back to its last good block: %w", path, err)
		}
	}
	w.blocks, w.end, w.ver = ix.Blocks, ix.ValidEnd, ix.Version
	return w, nil
}

// appendBlock writes a block and fsyncs it before returning, which is the
// ordering the store relies on: a block is durable before the WAL entries it
// replaces may be dropped.
//
// bloom is the block's filter, written after the compressed data; it is
// dropped when the file is a v1 chunk, whose layout has no room for it.
func (w *chunkWriter) appendBlock(m BlockMeta, comp, bloom []byte) (BlockMeta, error) {
	if w.ver < 2 {
		bloom = nil
	}
	m.Offset, m.ver, m.BloomLen = w.end, w.ver, uint32(len(bloom))
	hsz := hdrSize(w.ver)
	buf := make([]byte, hsz, hsz+len(comp)+len(bloom))
	putBlockHeader(buf, m)
	buf = append(buf, comp...)
	buf = append(buf, bloom...)
	if _, err := w.f.WriteAt(buf, w.end); err != nil {
		// A partial block is a torn tail; cut it off so the next one lands
		// where the reader expects.
		_ = w.f.Truncate(w.end)
		return BlockMeta{}, fmt.Errorf("logstore: writing block: %w", err)
	}
	if err := w.sync(); err != nil {
		return BlockMeta{}, fmt.Errorf("logstore: syncing chunk: %w", err)
	}
	w.blocks = append(w.blocks, m)
	w.end = m.end()
	return m, nil
}

// seal writes the footer index and syncs. The chunk stays appendable: the next
// openChunk cuts the footer off again.
func (w *chunkWriter) seal() error {
	if _, err := w.f.WriteAt(encodeFooter(w.blocks, w.end, w.ver), w.end); err != nil {
		_ = w.f.Truncate(w.end)
		return fmt.Errorf("logstore: writing footer: %w", err)
	}
	return w.sync()
}

func (w *chunkWriter) sync() error {
	if w.noSync {
		return nil
	}
	return w.f.Sync()
}

func (w *chunkWriter) close() error { return w.f.Close() }

func crcOf(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
