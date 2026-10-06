package tracestore

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/fnv"
	"strings"
)

// Key prefixes. One byte each, so a prefix scan of one family never touches another.
const (
	prefixSpan     byte = 's' // s <trace 16B> <span 8B>                          -> zstd(JSON span)
	prefixEntry    byte = 'e' // e <env> 0 <service> 0 <~start 8B> <trace> <span> -> summary JSON (top-level spans only)
	prefixResource byte = 'r' // r <env> 0 <service> 0 <resource hash 8B> <~start> <trace> <span> -> empty
	prefixError    byte = 'x' // x <env> 0 <service> 0 <~start> <trace> <span>     -> empty (entry spans of traces with an error)
	prefixEdge     byte = 'g' // g <hour 4B> <env> 0 <parent> 0 <child>            -> merge counter
	prefixSeen     byte = 't' // t <hour 4B> <trace 16B>                          -> empty (first seen, for retention)
	prefixService  byte = 'v' // v <env> 0 <service>                              -> empty (which pairs exist)
)

const (
	traceLen = 16
	spanLen  = 8
	// suffixLen is what follows the (env, service[, resource]) prefix of an
	// index key: ~start, trace id, span id. It is also the cursor.
	suffixLen = 8 + traceLen + spanLen
)

var errBadKey = errors.New("tracestore: malformed key")

// invert flips a start time so that a forward scan visits newest first. Start
// times are positive microseconds (wire.ValidateSpan), so the flip is monotone
// decreasing over the whole range and big-endian byte order equals time order.
func invert(ts int64) uint64 { return ^uint64(ts) }

func uninvert(u uint64) int64 { return int64(^u) }

// clean removes the one byte the key layout reserves as a separator. env and
// service come from an app; a NUL in one must not be able to shift where the
// other begins.
func clean(s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "_")
}

func decodeID(s string, n int) ([]byte, bool) {
	if len(s) != 2*n {
		return nil, false
	}
	b, err := hex.DecodeString(s)
	return b, err == nil
}

// resourceHash is the 8-byte name of a resource in the 'r' index. It is not
// trusted to be unique: a collision makes the search read a few extra entries,
// and the reader compares the resource itself before returning one.
func resourceHash(resource string) [8]byte {
	h := fnv.New64a()
	_, _ = h.Write([]byte(resource))
	var out [8]byte
	binary.BigEndian.PutUint64(out[:], h.Sum64())
	return out
}

func spanKey(trace, span []byte) []byte {
	k := make([]byte, 0, 1+traceLen+spanLen)
	k = append(k, prefixSpan)
	k = append(k, trace...)
	return append(k, span...)
}

func tracePrefix(trace []byte) []byte { return append([]byte{prefixSpan}, trace...) }

// pairPrefix is the start of every index key of one (env, service).
func pairPrefix(kind byte, env, service string) []byte {
	k := make([]byte, 0, 3+len(env)+len(service))
	k = append(k, kind)
	k = append(k, clean(env)...)
	k = append(k, 0)
	k = append(k, clean(service)...)
	return append(k, 0)
}

func suffix(start int64, trace, span []byte) []byte {
	s := make([]byte, suffixLen)
	binary.BigEndian.PutUint64(s, invert(start))
	copy(s[8:], trace)
	copy(s[8+traceLen:], span)
	return s
}

func entryKey(env, service string, start int64, trace, span []byte) []byte {
	return append(pairPrefix(prefixEntry, env, service), suffix(start, trace, span)...)
}

func errorKey(env, service string, start int64, trace, span []byte) []byte {
	return append(pairPrefix(prefixError, env, service), suffix(start, trace, span)...)
}

func resourcePrefix(env, service string, h [8]byte) []byte {
	return append(pairPrefix(prefixResource, env, service), h[:]...)
}

func resourceKey(env, service, resource string, start int64, trace, span []byte) []byte {
	return append(resourcePrefix(env, service, resourceHash(resource)), suffix(start, trace, span)...)
}

// splitSuffix reads the last suffixLen bytes of an index key.
func splitSuffix(key []byte) (start int64, trace, span []byte, err error) {
	if len(key) < suffixLen+1 {
		return 0, nil, nil, errBadKey
	}
	s := key[len(key)-suffixLen:]
	return uninvert(binary.BigEndian.Uint64(s)), s[8 : 8+traceLen], s[8+traceLen:], nil
}

// splitPair reads (env, service) from an 'e' or 'x' key, or the same from a 'v' key (no suffix).
func splitPair(key []byte, hasSuffix bool) (env, service string, err error) {
	if len(key) < 2 {
		return "", "", errBadKey
	}
	body := key[1:]
	if hasSuffix {
		if len(body) < suffixLen {
			return "", "", errBadKey
		}
		body = body[:len(body)-suffixLen]
	}
	i := bytes.IndexByte(body, 0)
	if i < 0 {
		return "", "", errBadKey
	}
	env, rest := string(body[:i]), body[i+1:]
	if hasSuffix {
		if len(rest) == 0 || rest[len(rest)-1] != 0 {
			return "", "", errBadKey
		}
		rest = rest[:len(rest)-1]
	}
	return env, string(rest), nil
}

func serviceKey(env, service string) []byte {
	k := make([]byte, 0, 2+len(env)+len(service))
	k = append(k, prefixService)
	k = append(k, clean(env)...)
	k = append(k, 0)
	return append(k, clean(service)...)
}

func hourOf(us int64) uint32 { return uint32(us / 3_600_000_000) }

func edgeKey(hour uint32, env, parent, child string) []byte {
	k := make([]byte, 0, 8+len(env)+len(parent)+len(child))
	k = append(k, prefixEdge)
	k = binary.BigEndian.AppendUint32(k, hour)
	k = append(k, clean(env)...)
	k = append(k, 0)
	k = append(k, clean(parent)...)
	k = append(k, 0)
	return append(k, clean(child)...)
}

func splitEdgeKey(key []byte) (hour uint32, env, parent, child string, err error) {
	if len(key) < 1+4+2 {
		return 0, "", "", "", errBadKey
	}
	hour = binary.BigEndian.Uint32(key[1:5])
	parts := bytes.Split(key[5:], []byte{0})
	if len(parts) != 3 {
		return 0, "", "", "", errBadKey
	}
	return hour, string(parts[0]), string(parts[1]), string(parts[2]), nil
}

func seenKey(hour uint32, trace []byte) []byte {
	k := make([]byte, 0, 1+4+traceLen)
	k = append(k, prefixSeen)
	k = binary.BigEndian.AppendUint32(k, hour)
	return append(k, trace...)
}

// successor is the smallest key greater than every key with the given prefix.
func successor(prefix []byte) []byte {
	out := append([]byte(nil), prefix...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] < 0xFF {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}
