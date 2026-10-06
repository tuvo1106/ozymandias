package tracestore

import (
	"bytes"
	"testing"

	"pgregory.net/rapid"
)

func id(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

// Byte order of an entry key within one (env, service) is newest-first time order.
func TestEntryKeys_ByteOrderIsNewestFirst(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := rapid.Int64Range(1, 1<<60).Draw(t, "a")
		b := rapid.Int64Range(1, 1<<60).Draw(t, "b")
		ka := entryKey("dev", "api", a, id(1, 16), id(1, 8))
		kb := entryKey("dev", "api", b, id(2, 16), id(2, 8))
		switch {
		case a > b && bytes.Compare(ka, kb) >= 0, a < b && bytes.Compare(ka, kb) <= 0:
			t.Fatalf("start %d vs %d: key order disagrees with time order", a, b)
		}
	})
}

func TestKeys_RoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		env := clean(rapid.StringN(0, 8, 20).Draw(t, "env"))
		svc := clean(rapid.StringN(1, 8, 20).Draw(t, "svc"))
		start := rapid.Int64Range(1, 1<<60).Draw(t, "start")
		tr, sp := id(0xab, 16), id(0xcd, 8)
		for _, k := range [][]byte{entryKey(env, svc, start, tr, sp), errorKey(env, svc, start, tr, sp)} {
			gs, gt, gp, err := splitSuffix(k)
			if err != nil || gs != start || !bytes.Equal(gt, tr) || !bytes.Equal(gp, sp) {
				t.Fatalf("suffix: %v %d", err, gs)
			}
			ge, gsvc, err := splitPair(k, true)
			if err != nil || ge != env || gsvc != svc {
				t.Fatalf("pair: %q %q %v, want %q %q", ge, gsvc, err, env, svc)
			}
		}
		ge, gs, err := splitPair(serviceKey(env, svc), false)
		if err != nil || ge != env || gs != svc {
			t.Fatalf("service key: %q %q %v", ge, gs, err)
		}
		h := uint32(rapid.IntRange(0, 1<<20).Draw(t, "hour"))
		gh, e2, p2, c2, err := splitEdgeKey(edgeKey(h, env, "p"+svc, "c"+svc))
		if err != nil || gh != h || e2 != env || p2 != "p"+svc || c2 != "c"+svc {
			t.Fatalf("edge key: %v", err)
		}
	})
}

// A NUL in env or service must not move the boundary between them.
func TestKeys_NulInNamesCannotShiftTheBoundary(t *testing.T) {
	k := entryKey("de\x00v", "a\x00pi", 5, id(1, 16), id(1, 8))
	env, svc, err := splitPair(k, true)
	if err != nil || env != "de_v" || svc != "a_pi" {
		t.Fatalf("%q %q %v", env, svc, err)
	}
	if !bytes.HasPrefix(k, pairPrefix(prefixEntry, "de_v", "a_pi")) {
		t.Error("the cleaned names do not form the prefix")
	}
}

func TestKeys_MalformedNeverPanic(t *testing.T) {
	for _, k := range [][]byte{nil, {'e'}, {'e', 0}, {'e', 0, 0}, id('e', 40), {'g', 0, 0, 0}, {'g', 0, 0, 0, 1, 0}} {
		_, _, _, _ = splitSuffix(k)
		_, _, _ = splitPair(k, true)
		_, _, _ = splitPair(k, false)
		_, _, _, _, _ = splitEdgeKey(k)
	}
}

func FuzzKeyDecoders(f *testing.F) {
	f.Add(entryKey("dev", "api", 5, id(1, 16), id(1, 8)))
	f.Add(edgeKey(3, "dev", "a", "b"))
	f.Fuzz(func(t *testing.T, k []byte) {
		_, _, _, _ = splitSuffix(k)
		_, _, _ = splitPair(k, true)
		_, _, _ = splitPair(k, false)
		_, _, _, _, _ = splitEdgeKey(k)
		_, _ = decodeEdge(k)
	})
}

func TestEdgeValue_RoundTripAndMergeIsOrderIndependent(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 10).Draw(t, "n")
		vals := make([]edgeVal, n)
		var want edgeVal
		for i := range vals {
			vals[i] = edgeVal{uint64(rapid.IntRange(0, 1000).Draw(t, "c")), uint64(rapid.IntRange(0, 100).Draw(t, "e")), uint64(rapid.IntRange(0, 1<<30).Draw(t, "d"))}
			want.calls += vals[i].calls
			want.errors += vals[i].errors
			want.durSum += vals[i].durSum
			if got, ok := decodeEdge(vals[i].encode()); !ok || got != vals[i] {
				t.Fatalf("round trip %+v -> %+v", vals[i], got)
			}
		}
		for _, newer := range []bool{true, false} {
			m := &edgeMerge{}
			for _, v := range vals {
				var err error
				if newer {
					err = m.MergeNewer(v.encode())
				} else {
					err = m.MergeOlder(v.encode())
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			out, _, _ := m.Finish(true)
			if got, ok := decodeEdge(out); !ok || got != want {
				t.Fatalf("merge = %+v, want %+v", got, want)
			}
		}
	})
}

func TestEdgeValue_GarbageOperandIsIgnoredNotFatal(t *testing.T) {
	m := &edgeMerge{}
	_ = m.MergeNewer(edgeVal{1, 0, 5}.encode())
	_ = m.MergeNewer([]byte{0xff, 0xff})
	out, _, _ := m.Finish(true)
	if got, _ := decodeEdge(out); got.calls != 1 {
		t.Errorf("%+v", got)
	}
}

func TestSuccessor(t *testing.T) {
	for in, want := range map[string]string{"a": "b", "a\xff": "b", "ab": "ac"} {
		if got := string(successor([]byte(in))); got != want {
			t.Errorf("successor(%q) = %q, want %q", in, got, want)
		}
	}
	if successor([]byte{0xff, 0xff}) != nil {
		t.Error("successor of all-0xff must be nil (unbounded)")
	}
}

// The layout in docs/formats/tracestore-keys.md, byte for byte. A change here is a format change.
func TestKeys_LayoutMatchesTheFormatDoc(t *testing.T) {
	trace, span := id(0x11, 16), id(0x22, 8)
	got := entryKey("dev", "api", 1, trace, span)
	want := append([]byte("edev\x00api\x00"), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe) // ^1
	want = append(append(want, trace...), span...)
	if !bytes.Equal(got, want) {
		t.Errorf("entry key\n got %x\nwant %x", got, want)
	}
	if k := edgeKey(0x01020304, "dev", "a", "b"); !bytes.Equal(k, []byte("g\x01\x02\x03\x04dev\x00a\x00b")) {
		t.Errorf("edge key %x", k)
	}
	if k := seenKey(7, trace); !bytes.Equal(k, append([]byte("t\x00\x00\x00\x07"), trace...)) {
		t.Errorf("seen key %x", k)
	}
	if k := serviceKey("dev", "api"); string(k) != "vdev\x00api" {
		t.Errorf("service key %q", k)
	}
	if k := spanKey(trace, span); !bytes.Equal(k, append(append([]byte("s"), trace...), span...)) {
		t.Errorf("span key %x", k)
	}
}
