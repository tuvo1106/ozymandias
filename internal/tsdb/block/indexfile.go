package block

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/index"
)

// IndexFilename locates the block's series and postings.
const IndexFilename = "index.dat"

// index.dat layout:
//
//	magic u32 | version u8
//	symbols   uvarint n | (uvarint len | bytes)…      sorted, unique
//	series    uvarint n | per series, ordered by key:
//	            uvarint metricSym
//	            uvarint ntags | (uvarint keySym | uvarint valSym)…
//	            uvarint nchunks | (varint minT | uvarint maxT-minT |
//	                               uvarint offset | uvarint recLen)…
//	postings  per (key,value): uvarint n | uvarint firstID | (uvarint delta)…
//	table     uvarint n | (uvarint keySym | uvarint valSym | uvarint offset)…
//	TOC       u64 symbols | u64 series | u64 postings | u64 table
//	crc32c    u32 over everything above
//
// Two ideas carry most of the weight. *Symbols*: tag strings repeat across
// series — `env`, `prod`, a handful of route names — so each distinct string
// is stored once and referenced by its ordinal, which is the difference
// between a block whose index rivals its data and one whose index is a tenth
// of it. *The offset table*: a sorted (key,value) → postings-offset map, so a
// lookup is a binary search rather than a scan, and the postings themselves
// are only touched for the pairs a query actually mentions.
//
// Symbols are sorted, so a symbol's ordinal orders the same way the string
// does. The table can therefore be searched on ordinals, without dereferencing
// a single string.
const (
	indexMagic   uint32 = 0x4f5a4958 // "OZIX"
	indexVersion uint8  = 1
	indexHeadLen        = 5
	tocLen              = 8 * 4
)

// chunkRef locates one chunk of one series inside chunks.dat.
type chunkRef struct {
	minT, maxT  int64
	off, recLen uint64
}

// blockSeries is a series as stored: identity plus where its chunks are.
type blockSeries struct {
	ref    tsdb.SeriesRef
	chunks []chunkRef
}

// --- writing ---------------------------------------------------------------

// indexWriter builds index.dat in memory and writes it once. A block's index
// is small next to its chunks (symbols see to that), and building it in one
// pass is much simpler than the two-pass, seek-back layout a large index would
// need.
type indexWriter struct {
	series []blockSeries
}

func (w *indexWriter) add(s blockSeries) { w.series = append(w.series, s) }

func (w *indexWriter) writeTo(dir string) error {
	// Series are stored in key order so that a block's ids (their ordinals)
	// are a deterministic function of its contents: two compactions of the
	// same input produce byte-identical blocks, which is what makes golden
	// tests and `diff` on two blocks meaningful.
	sort.Slice(w.series, func(i, j int) bool {
		return w.series[i].ref.Key() < w.series[j].ref.Key()
	})

	syms, symID := buildSymbols(w.series)
	buf := make([]byte, 0, 1<<12)
	buf = binary.BigEndian.AppendUint32(buf, indexMagic)
	buf = append(buf, indexVersion)

	symbolsOff := uint64(len(buf))
	buf = binary.AppendUvarint(buf, uint64(len(syms)))
	for _, s := range syms {
		buf = binary.AppendUvarint(buf, uint64(len(s)))
		buf = append(buf, s...)
	}

	seriesOff := uint64(len(buf))
	buf = binary.AppendUvarint(buf, uint64(len(w.series)))
	for _, s := range w.series {
		buf = binary.AppendUvarint(buf, symID[s.ref.Metric])
		buf = binary.AppendUvarint(buf, uint64(len(s.ref.Tags)))
		for _, t := range s.ref.Tags {
			buf = binary.AppendUvarint(buf, symID[t.Key])
			buf = binary.AppendUvarint(buf, symID[t.Value])
		}
		buf = binary.AppendUvarint(buf, uint64(len(s.chunks)))
		for _, c := range s.chunks {
			buf = binary.AppendVarint(buf, c.minT)
			buf = binary.AppendUvarint(buf, uint64(c.maxT-c.minT))
			buf = binary.AppendUvarint(buf, c.off)
			buf = binary.AppendUvarint(buf, c.recLen)
		}
	}

	// Postings, in (key,value) order so the table below is sorted by
	// construction and a lookup is a binary search.
	postingsOff := uint64(len(buf))
	lists := invert(w.series)
	type entry struct{ keySym, valSym, off uint64 }
	table := make([]entry, 0, len(lists))
	for _, l := range lists {
		table = append(table, entry{symID[l.key], symID[l.value], uint64(len(buf))})
		buf = binary.AppendUvarint(buf, uint64(len(l.ids)))
		var prev uint64
		for i, id := range l.ids {
			if i == 0 {
				buf = binary.AppendUvarint(buf, id)
			} else {
				// Deltas: ids are sorted and usually dense, so almost every
				// delta is a single byte where the id itself would be two or
				// three. On a block with many series the postings are the
				// largest part of the index, and this roughly halves them.
				buf = binary.AppendUvarint(buf, id-prev)
			}
			prev = id
		}
	}

	tableOff := uint64(len(buf))
	buf = binary.AppendUvarint(buf, uint64(len(table)))
	for _, e := range table {
		buf = binary.AppendUvarint(buf, e.keySym)
		buf = binary.AppendUvarint(buf, e.valSym)
		buf = binary.AppendUvarint(buf, e.off)
	}

	for _, off := range []uint64{symbolsOff, seriesOff, postingsOff, tableOff} {
		buf = binary.BigEndian.AppendUint64(buf, off)
	}
	buf = binary.BigEndian.AppendUint32(buf, crc32.Checksum(buf, castagnoli))
	return writeFileSync(filepath.Join(dir, IndexFilename), buf)
}

// buildSymbols collects every distinct string in the block, sorted, and the
// map from string to ordinal.
func buildSymbols(series []blockSeries) ([]string, map[string]uint64) {
	set := map[string]struct{}{index.MetricName: {}}
	for _, s := range series {
		set[s.ref.Metric] = struct{}{}
		for _, t := range s.ref.Tags {
			set[t.Key] = struct{}{}
			set[t.Value] = struct{}{}
		}
	}
	syms := make([]string, 0, len(set))
	for s := range set {
		syms = append(syms, s)
	}
	sort.Strings(syms)
	ids := make(map[string]uint64, len(syms))
	for i, s := range syms {
		ids[s] = uint64(i)
	}
	return syms, ids
}

type postingList struct {
	key, value string
	ids        []uint64
}

// invert builds the postings lists, including the metric name under the
// [index.MetricName] pseudo key so that "which series are named X" is the same
// kind of lookup as any tag.
func invert(series []blockSeries) []postingList {
	m := map[string]map[string][]uint64{}
	add := func(k, v string, id uint64) {
		vals, ok := m[k]
		if !ok {
			vals = map[string][]uint64{}
			m[k] = vals
		}
		vals[v] = append(vals[v], id)
	}
	for id, s := range series {
		add(index.MetricName, s.ref.Metric, uint64(id))
		for _, t := range s.ref.Tags {
			add(t.Key, t.Value, uint64(id))
		}
	}
	out := make([]postingList, 0, len(m))
	for k, vals := range m {
		for v, ids := range vals {
			out = append(out, postingList{k, v, ids})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].key != out[j].key {
			return out[i].key < out[j].key
		}
		return out[i].value < out[j].value
	})
	return out
}

// --- reading ---------------------------------------------------------------

// indexReader is a parsed index.dat. The whole file is read at open: it is the
// small half of a block, and holding it lets every lookup be a slice operation.
// A production store mmaps it instead and lets the page cache evict the parts
// nobody queries — the same layout, with a different memory story.
type indexReader struct {
	symbols []string
	series  []blockSeries
	// postings is the raw section; entries index into it.
	postings []byte
	table    []tableEntry
	all      []uint64
	// symID is the reverse of symbols, for turning a matcher's strings into
	// ordinals once per query instead of once per comparison.
	symID map[string]uint64
}

type tableEntry struct{ keySym, valSym, off uint64 }

func openIndexReader(dir string) (*indexReader, error) {
	b, err := os.ReadFile(filepath.Join(dir, IndexFilename))
	if err != nil {
		return nil, err
	}
	if len(b) < indexHeadLen+tocLen+4 {
		return nil, fmt.Errorf("block: %s is too short to be an index", dir)
	}
	if got := binary.BigEndian.Uint32(b); got != indexMagic {
		return nil, fmt.Errorf("block: %s is not an index file (magic %#x)", dir, got)
	}
	if b[4] != indexVersion {
		return nil, fmt.Errorf("block: index version %d, this build reads %d", b[4], indexVersion)
	}
	body, sum := b[:len(b)-4], binary.BigEndian.Uint32(b[len(b)-4:])
	if got := crc32.Checksum(body, castagnoli); got != sum {
		return nil, fmt.Errorf("block: %s/%s is corrupt (crc %#x, want %#x)",
			dir, IndexFilename, got, sum)
	}
	toc := body[len(body)-tocLen:]
	offs := make([]uint64, 4)
	for i := range offs {
		offs[i] = binary.BigEndian.Uint64(toc[i*8:])
		if offs[i] > uint64(len(body)) {
			return nil, fmt.Errorf("block: index TOC points outside the file")
		}
	}
	r := &indexReader{postings: body}

	d := &decoder{b: body[offs[0]:]}
	if err := r.readSymbols(d); err != nil {
		return nil, err
	}
	d = &decoder{b: body[offs[1]:]}
	if err := r.readSeries(d); err != nil {
		return nil, err
	}
	d = &decoder{b: body[offs[3]:]}
	if err := r.readTable(d); err != nil {
		return nil, err
	}
	r.all = make([]uint64, len(r.series))
	for i := range r.all {
		r.all[i] = uint64(i)
	}
	return r, nil
}

func (r *indexReader) readSymbols(d *decoder) error {
	n := d.uvarint()
	if d.err != nil || n > uint64(len(d.b)) {
		return fmt.Errorf("block: index claims %d symbols: %w", n, d.err)
	}
	r.symbols = make([]string, 0, n)
	r.symID = make(map[string]uint64, n)
	for i := uint64(0); i < n; i++ {
		s := d.str()
		if d.err != nil {
			return fmt.Errorf("block: reading symbol %d: %w", i, d.err)
		}
		r.symID[s] = uint64(len(r.symbols))
		r.symbols = append(r.symbols, s)
	}
	return nil
}

func (r *indexReader) sym(i uint64) (string, error) {
	if i >= uint64(len(r.symbols)) {
		return "", fmt.Errorf("block: symbol %d of %d", i, len(r.symbols))
	}
	return r.symbols[i], nil
}

func (r *indexReader) readSeries(d *decoder) error {
	n := d.uvarint()
	if d.err != nil || n > uint64(len(d.b)) {
		return fmt.Errorf("block: index claims %d series: %w", n, d.err)
	}
	r.series = make([]blockSeries, 0, n)
	for i := uint64(0); i < n; i++ {
		var s blockSeries
		metric, err := r.sym(d.uvarint())
		if err != nil {
			return err
		}
		s.ref.Metric = metric
		ntags := d.uvarint()
		if d.err != nil || ntags > uint64(len(d.b)) {
			return fmt.Errorf("block: series %d claims %d tags: %w", i, ntags, d.err)
		}
		s.ref.Tags = make([]tsdb.Tag, ntags)
		for j := range s.ref.Tags {
			k, err := r.sym(d.uvarint())
			if err != nil {
				return err
			}
			v, err := r.sym(d.uvarint())
			if err != nil {
				return err
			}
			s.ref.Tags[j] = tsdb.Tag{Key: k, Value: v}
		}
		nchunks := d.uvarint()
		if d.err != nil || nchunks > uint64(len(d.b)) {
			return fmt.Errorf("block: series %d claims %d chunks: %w", i, nchunks, d.err)
		}
		s.chunks = make([]chunkRef, nchunks)
		for j := range s.chunks {
			minT := d.varint()
			s.chunks[j] = chunkRef{
				minT:   minT,
				maxT:   minT + int64(d.uvarint()),
				off:    d.uvarint(),
				recLen: d.uvarint(),
			}
		}
		if d.err != nil {
			return fmt.Errorf("block: reading series %d: %w", i, d.err)
		}
		r.series = append(r.series, s)
	}
	return nil
}

func (r *indexReader) readTable(d *decoder) error {
	n := d.uvarint()
	if d.err != nil || n > uint64(len(d.b)) {
		return fmt.Errorf("block: index claims %d postings lists: %w", n, d.err)
	}
	r.table = make([]tableEntry, n)
	for i := range r.table {
		r.table[i] = tableEntry{keySym: d.uvarint(), valSym: d.uvarint(), off: d.uvarint()}
		if r.table[i].off >= uint64(len(r.postings)) {
			return fmt.Errorf("block: postings entry %d points outside the file", i)
		}
	}
	return d.err
}

// Postings implements [index.Lookup].
func (r *indexReader) Postings(key, value string) []uint64 {
	k, ok := r.symID[key]
	if !ok {
		return nil
	}
	v, ok := r.symID[value]
	if !ok {
		return nil
	}
	i := sort.Search(len(r.table), func(i int) bool {
		e := r.table[i]
		return e.keySym > k || (e.keySym == k && e.valSym >= v)
	})
	if i == len(r.table) || r.table[i].keySym != k || r.table[i].valSym != v {
		return nil
	}
	return r.readPostings(r.table[i].off)
}

// maxPostingsLen bounds what a corrupt count may claim. One id is at least one
// byte, so a list longer than the section cannot be real.
func (r *indexReader) readPostings(off uint64) []uint64 {
	d := &decoder{b: r.postings[off:]}
	n := d.uvarint()
	if d.err != nil || n > uint64(len(d.b))+1 {
		return nil
	}
	out := make([]uint64, 0, n)
	var prev uint64
	for i := uint64(0); i < n; i++ {
		delta := d.uvarint()
		if d.err != nil {
			return out
		}
		if i == 0 {
			prev = delta
		} else {
			prev += delta
		}
		out = append(out, prev)
	}
	return out
}

// TagValues implements [index.Lookup]: the values of one key, found as the
// contiguous run of table entries sharing its symbol.
func (r *indexReader) TagValues(key string) []string {
	k, ok := r.symID[key]
	if !ok {
		return nil
	}
	i := sort.Search(len(r.table), func(i int) bool { return r.table[i].keySym >= k })
	var out []string
	for ; i < len(r.table) && r.table[i].keySym == k; i++ {
		v, err := r.sym(r.table[i].valSym)
		if err != nil {
			return out
		}
		out = append(out, v)
	}
	return out
}

// AllSeries implements [index.Lookup]. Ids are ordinals, so the universe is
// 0..n-1 and needs no storage.
func (r *indexReader) AllSeries() []uint64 { return r.all }

// decoder reads the encodings above, remembering the first error.
type decoder struct {
	b   []byte
	err error
}

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err = fmt.Errorf("block: truncated uvarint")
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) varint() int64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Varint(d.b)
	if n <= 0 {
		d.err = fmt.Errorf("block: truncated varint")
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) str() string {
	n := d.uvarint()
	if d.err != nil {
		return ""
	}
	if n > uint64(len(d.b)) {
		d.err = fmt.Errorf("block: string of %d bytes exceeds the %d remaining", n, len(d.b))
		return ""
	}
	s := string(d.b[:n])
	d.b = d.b[n:]
	return s
}

// TagKeys implements [index.Lookup]: the distinct keys in the offset table,
// which is sorted by key symbol, so they come out in order without a sort.
func (r *indexReader) TagKeys() []string {
	var out []string
	var last uint64
	for i, e := range r.table {
		if i > 0 && e.keySym == last {
			continue
		}
		last = e.keySym
		k, err := r.sym(e.keySym)
		if err != nil {
			return out
		}
		out = append(out, k)
	}
	return out
}
