package logstore

import (
	"encoding/binary"
	"hash/crc32"
	"strings"

	"github.com/tuvo1106/ozymandias/internal/query/logql"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// A block's bloom filter answers one question without decompressing the block:
// "can this word occur in any of these logs?" A "no" is certain, a "yes" is
// not, and that asymmetry is the whole design. The store's ordinary search
// decompresses a block to find out (the cost of a bare-word search in a
// label-indexed store: every block of every selected stream), and a bloom
// filter turns most of those decompressions into a small read.
//
// # Why trigrams, not words
//
// A free-text term is a case-insensitive SUBSTRING: `time` finds "timeout".
// A filter of whole words cannot answer that without changing what a search
// means, so the filter holds every three-byte window of the block's
// lowercased text instead. A substring of three or more bytes occurs in a
// text only if all its windows do, so absent windows prove absence. A term
// with a wildcard contributes each literal run, and a term shorter than three
// bytes (or a number, a NOT, a label) constrains nothing and skips nothing.
// What this buys is soundness for the existing semantics; what it costs is
// size: distinct trigrams outnumber distinct words several times over, which
// is why the filter is sized by the distinct windows actually present and
// accepts a few percent of false positives (a window absent from the block is
// reported present about one time in thirty, and a term of n bytes needs n-2
// of them to be wrong at once).
//
// What goes in is everything a free-text or attribute term can match: the
// message and every string, number and boolean attribute value, lowercased
// the way the matcher lowercases. Attribute NAMES are not in it, and neither
// are labels (the index answers those).
//
// # The blob
//
//	k u8 | bits (a multiple of 8) | crc32c u32 (of k and bits)
//
// The checksum is the safety property. A flipped bit that turns a 1 into a 0
// would make the filter deny something that is there, and a skipped block is
// a silently missing log. So a blob that does not verify is ignored, and the
// block is read the slow way.
const (
	bloomK            = 4
	bloomBitsPerItem  = 8
	minBloomBytes     = 8
	maxBloomBytes     = 64 << 10
	bloomTrailerBytes = 4
)

// trigram packs three bytes; windows is how a text becomes its windows.
func trigram(a, b, c byte) uint32 { return uint32(a)<<16 | uint32(b)<<8 | uint32(c) }

// probes derives the filter's bit positions for a trigram from one 64-bit
// mix: double hashing (h1 + i*h2) gives k positions from two values, which is
// as good as k independent hashes for a bloom filter.
func probes(t uint32, m uint64, f func(bit uint64) bool) bool {
	x := uint64(t) * 0x9E3779B97F4A7C15
	x ^= x >> 29
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 32
	h1, h2 := uint64(uint32(x)), uint64(uint32(x>>32))|1
	for i := uint64(0); i < bloomK; i++ {
		if !f((h1 + i*h2) % m) {
			return false
		}
	}
	return true
}

// textsOf lists the strings of a log a free-text or attribute term can match.
func textsOf(l *wire.Log, add func(string)) {
	add(l.Message)
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			add(v)
		case map[string]any:
			for _, e := range v {
				walk(e)
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		case nil:
		default:
			add(logql.ValueText(v))
		}
	}
	walk(l.Attrs)
}

// buildBloom builds the filter blob for a block's entries. It returns nil if a
// body cannot be decoded: no filter is always safe, a wrong one is not.
func buildBloom(entries []rawEntry) []byte {
	seen := make(map[uint32]struct{}, 1024)
	for _, e := range entries {
		l, err := decodeLog(e.Body)
		if err != nil {
			return nil
		}
		textsOf(l, func(s string) {
			s = strings.ToLower(s)
			for i := 0; i+2 < len(s); i++ {
				seen[trigram(s[i], s[i+1], s[i+2])] = struct{}{}
			}
		})
	}
	n := len(seen)
	nbytes := min(max((n*bloomBitsPerItem+7)/8, minBloomBytes), maxBloomBytes)
	blob := make([]byte, 1+nbytes+bloomTrailerBytes)
	blob[0] = bloomK
	bits := blob[1 : 1+nbytes]
	m := uint64(nbytes) * 8
	for t := range seen {
		probes(t, m, func(bit uint64) bool { bits[bit/8] |= 1 << (bit % 8); return true })
	}
	binary.BigEndian.PutUint32(blob[1+nbytes:], crc32.Checksum(blob[:1+nbytes], castagnoli))
	return blob
}

// bloomView is a verified filter.
type bloomView struct {
	bits []byte
	m    uint64
}

// parseBloom checks a blob and returns a view of it. ok is false for a blob
// that is the wrong shape or fails its checksum, which callers treat as "no
// filter".
func parseBloom(blob []byte) (bloomView, bool) {
	if len(blob) < 1+minBloomBytes+bloomTrailerBytes || blob[0] != bloomK {
		return bloomView{}, false
	}
	body := blob[:len(blob)-bloomTrailerBytes]
	if crc32.Checksum(body, castagnoli) != binary.BigEndian.Uint32(blob[len(body):]) {
		return bloomView{}, false
	}
	bits := body[1:]
	return bloomView{bits: bits, m: uint64(len(bits)) * 8}, true
}

func (b bloomView) has(t uint32) bool {
	return probes(t, b.m, func(bit uint64) bool { return b.bits[bit/8]&(1<<(bit%8)) != 0 })
}

// hasLiteral reports whether every window of lit may be present.
func (b bloomView) hasLiteral(lit string) bool {
	for i := 0; i+2 < len(lit); i++ {
		if !b.has(trigram(lit[i], lit[i+1], lit[i+2])) {
			return false
		}
	}
	return true
}

// mayMatch reports whether a block with this filter could hold a log
// satisfying need. True is "read it"; false is "certainly not".
func (b bloomView) mayMatch(need logql.Need) bool {
	if need.None() {
		return true
	}
	for _, alt := range need.Alts {
		ok := true
		for _, lit := range alt {
			if !b.hasLiteral(lit) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
