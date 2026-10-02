package resp

import (
	"bytes"
	"errors"
	"io"
	"math"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"

	"pgregory.net/rapid"
)

func read(t *testing.T, in string, lim Limits) (Value, error) {
	t.Helper()
	return NewReader(strings.NewReader(in), lim).Read()
}

func TestRead_EveryKind(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Value
	}{
		{"+OK\r\n", Value{Kind: SimpleString, Str: "OK"}},
		{"+\r\n", Value{Kind: SimpleString}},
		{"-ERR unknown command 'X'\r\n", Value{Kind: Error, Str: "ERR unknown command 'X'"}},
		{":0\r\n", Value{Kind: Integer}},
		{":-42\r\n", Value{Kind: Integer, Int: -42}},
		{":9223372036854775807\r\n", Value{Kind: Integer, Int: math.MaxInt64}},
		{"$5\r\nhello\r\n", Value{Kind: BulkString, Str: "hello"}},
		{"$0\r\n\r\n", Value{Kind: BulkString}},
		// Binary safe: CRLF inside a bulk string is content, not framing.
		{"$4\r\na\r\nb\r\n", Value{Kind: BulkString, Str: "a\r\nb"}},
		{"$-1\r\n", Value{Kind: BulkString, Null: true}},
		{"*0\r\n", Value{Kind: Array, Elems: []Value{}}},
		{"*-1\r\n", Value{Kind: Array, Null: true}},
		{"*2\r\n:1\r\n*1\r\n+x\r\n", Value{Kind: Array, Elems: []Value{
			{Kind: Integer, Int: 1}, {Kind: Array, Elems: []Value{{Kind: SimpleString, Str: "x"}}},
		}}},
		{"_\r\n", Value{Kind: Null}},
		{"#t\r\n", Value{Kind: Boolean, Bool: true}},
		{"#f\r\n", Value{Kind: Boolean}},
		{",1.5\r\n", Value{Kind: Double, Float: 1.5}},
		{",-2e3\r\n", Value{Kind: Double, Float: -2000}},
		{",inf\r\n", Value{Kind: Double, Float: math.Inf(1)}},
		{",-inf\r\n", Value{Kind: Double, Float: math.Inf(-1)}},
		{"(3492890328409238509324850943850943825024385\r\n", Value{Kind: BigNumber, Str: "3492890328409238509324850943850943825024385"}},
		{"=15\r\ntxt:Some string\r\n", Value{Kind: Verbatim, Format: "txt", Str: "Some string"}},
		{"!21\r\nSYNTAX invalid syntax\r\n", Value{Kind: BlobError, Str: "SYNTAX invalid syntax"}},
		{"%1\r\n+k\r\n:1\r\n", Value{Kind: Map, Elems: []Value{{Kind: SimpleString, Str: "k"}, {Kind: Integer, Int: 1}}}},
		{"~1\r\n#t\r\n", Value{Kind: Set, Elems: []Value{{Kind: Boolean, Bool: true}}}},
		{">1\r\n+m\r\n", Value{Kind: Push, Elems: []Value{{Kind: SimpleString, Str: "m"}}}},
	} {
		got, err := read(t, tc.in, Limits{})
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q:\n got %+v\nwant %+v", tc.in, got, tc.want)
		}
	}
	// NaN is not equal to itself, so it cannot go through DeepEqual.
	if v, err := read(t, ",nan\r\n", Limits{}); err != nil || !math.IsNaN(v.Float) {
		t.Errorf(",nan: %+v, %v", v, err)
	}
}

func TestRead_Malformed(t *testing.T) {
	for _, in := range []string{
		"\r\n",     // no type byte
		"?x\r\n",   // unknown type byte
		"+OK\n",    // LF without CR
		"+OK\rX\n", // CR not before LF
		"+\r\r\n",  // a CR of its own inside a line (found by FuzzRead)
		"-E\rR\r\n",
		":\r\n",    // empty integer
		":1.5\r\n", // not an integer
		":99999999999999999999\r\n",
		"$\r\n",          // empty length
		"$-2\r\n",        // negative, not null
		"$+3\r\nabc\r\n", // a sign is not a plain length
		"$3\r\nabcd\r\n", // content longer than announced
		"$3\r\nab\r\n\r\n",
		"*x\r\n",
		"*-2\r\n",
		"%-1\r\n", // only arrays and bulk strings may be null
		"~-1\r\n",
		"=-1\r\n",
		"=3\r\ntxt\r\n",     // verbatim without format:
		"=5\r\ntx:ab\r\n",   // format not three bytes
		"_x\r\n",            // null with content
		"#x\r\n",            // boolean not t/f
		",1.2.3\r\n",        // not a double
		",0x1p3\r\n",        // strconv would take a hex float; RESP does not
		",1_000\r\n",        // nor an underscore
		",Infinity\r\n",     // nor this spelling
		"(12a\r\n",          // big number with a letter
		"(\r\n",             // empty big number
		"*1\r\n+ok\n",       // bad framing inside an aggregate
		"*2\r\n:1\r\n?\r\n", // bad element after a good one
		"%1\r\n+k\r\n",      // a map missing its value
		"$3\r\nab",          // stream ends inside a bulk string
		"+OK",               // stream ends inside a line
		"*3\r\n:1\r\n",      // stream ends inside an aggregate
		"$18446744073709551616\r\n",
	} {
		v, err := read(t, in, Limits{})
		if err == nil {
			t.Errorf("%q: want an error, got %+v", in, v)
		}
	}
}

func TestRead_ErrorKinds(t *testing.T) {
	_, err := read(t, "?\r\n", Limits{})
	if !errors.Is(err, ErrProtocol) {
		t.Errorf("unknown type: %v, want ErrProtocol", err)
	}
	_, err = read(t, "$3\r\nab", Limits{})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("short bulk: %v, want ErrUnexpectedEOF", err)
	}
	_, err = read(t, "", Limits{})
	if !errors.Is(err, io.EOF) {
		t.Errorf("empty stream: %v, want io.EOF (a clean end between replies)", err)
	}
	_, err = read(t, "+OK", Limits{})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("stream ends mid-line: %v, want ErrUnexpectedEOF", err)
	}
	_, err = NewReader(iotest.ErrReader(errors.New("boom")), Limits{}).Read()
	if err == nil || err.Error() != "boom" {
		t.Errorf("reader error: %v, want it passed through", err)
	}
	// One byte, then a timeout: the reply is cut off after "$".
	_, err = NewReader(iotest.TimeoutReader(iotest.OneByteReader(strings.NewReader("$3\r\nabc\r\n"))), Limits{}).Read()
	if err == nil {
		t.Error("a timeout mid-reply must surface")
	}
}

func TestRead_Bounds(t *testing.T) {
	small := Limits{MaxBulk: 8, MaxElems: 3, MaxDepth: 2, MaxTotal: 4 * valueCost}
	for _, tc := range []struct {
		name, in string
	}{
		{"bulk over MaxBulk", "$9\r\n123456789\r\n"},
		{"verbatim over MaxBulk", "=13\r\ntxt:123456789\r\n"},
		{"huge claim, nothing sent", "$2000000000\r\n"},
		{"line over MaxBulk", "+" + strings.Repeat("x", 9) + "\r\n"},
		{"line with no CRLF at all", "+" + strings.Repeat("x", 100)},
		{"elements over MaxElems", "*4\r\n:1\r\n:1\r\n:1\r\n:1\r\n"},
		{"map pairs over MaxElems", "%4\r\n"},
		{"huge array claim", "*2000000000\r\n"},
		{"nesting over MaxDepth", "*1\r\n*1\r\n*1\r\n:1\r\n"},
		{"total over MaxTotal", "*3\r\n:1\r\n:2\r\n*1\r\n:3\r\n"},
	} {
		_, err := read(t, tc.in, small)
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: %v, want ErrTooLarge", tc.name, err)
		}
	}
	// And exactly at each limit is fine.
	for _, in := range []string{"$8\r\n12345678\r\n", "+12345678\r\n", "*3\r\n:1\r\n:1\r\n:1\r\n", "*1\r\n*1\r\n:1\r\n"} {
		if _, err := read(t, in, small); err != nil {
			t.Errorf("%q at the limit: %v", in, err)
		}
	}
}

// A claim the budget cannot pay for is refused before the allocation, which
// is the point: the refusal must not cost the memory. Measured in bytes, not
// allocations — one 1 GB make is a single allocation.
func TestRead_BudgetRefusesBeforeAllocating(t *testing.T) {
	lim := Limits{MaxBulk: 1 << 30, MaxTotal: 1 << 10}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := NewReader(strings.NewReader("$1000000000\r\n"), lim).Read()
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 1<<20 {
		t.Errorf("refusing a 1 GB claim allocated %d bytes", n)
	}
}

func TestRead_LongLineAcrossBufferBoundary(t *testing.T) {
	// bufio's default buffer is 4 KiB; a line longer than that is assembled
	// from several ReadSlice chunks.
	long := strings.Repeat("y", 10_000)
	v, err := read(t, "+"+long+"\r\n:7\r\n", Limits{})
	if err != nil || v.Str != long {
		t.Fatalf("got %d bytes, %v", len(v.Str), err)
	}
	// One byte at a time exercises the same path under short reads.
	r := NewReader(iotest.OneByteReader(strings.NewReader("*2\r\n+"+long+"\r\n$3\r\nabc\r\n")), Limits{})
	v, err = r.Read()
	if err != nil || len(v.Elems) != 2 || v.Elems[0].Str != long || v.Elems[1].Str != "abc" {
		t.Fatalf("one-byte reads: %v", err)
	}
}

func TestRead_Pipelined(t *testing.T) {
	r := NewReader(strings.NewReader("+OK\r\n:2\r\n$-1\r\n"), Limits{})
	for _, want := range []Value{{Kind: SimpleString, Str: "OK"}, {Kind: Integer, Int: 2}, {Kind: BulkString, Null: true}} {
		got, err := r.Read()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, %v; want %+v", got, err, want)
		}
	}
	if _, err := r.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last reply: %v, want EOF", err)
	}
}

func TestValue_ErrAndText(t *testing.T) {
	err := Value{Kind: Error, Str: "WRONGPASS invalid username-password pair"}.Err()
	var se *ServerError
	if !errors.As(err, &se) || se.Code() != "WRONGPASS" || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("err = %v", err)
	}
	if (&ServerError{Msg: "NOAUTH"}).Code() != "NOAUTH" {
		t.Error("a one-word message is its own code")
	}
	if (Value{Kind: BlobError, Str: "X y"}).Err() == nil || (Value{Kind: SimpleString}).Err() != nil {
		t.Error("Err: only error kinds are errors")
	}
	for _, tc := range []struct {
		v    Value
		want string
		ok   bool
	}{
		{Value{Kind: SimpleString, Str: "a"}, "a", true},
		{Value{Kind: BulkString, Str: "b"}, "b", true},
		{Value{Kind: Verbatim, Format: "txt", Str: "c"}, "c", true},
		{Value{Kind: BulkString, Null: true}, "", false},
		{Value{Kind: Integer, Int: 1}, "", false},
	} {
		if s, ok := tc.v.Text(); s != tc.want || ok != tc.ok {
			t.Errorf("Text(%+v) = %q, %v", tc.v, s, ok)
		}
	}
}

func TestKindString(t *testing.T) {
	for _, k := range []Kind{SimpleString, Error, Integer, BulkString, Array, Null, Boolean, Double, BigNumber, Verbatim, BlobError, Map, Set, Push} {
		if s := k.String(); s == "" || strings.HasPrefix(s, "kind(") {
			t.Errorf("%q has no name", byte(k))
		}
	}
	if Kind('?').String() != `kind('?')` {
		t.Errorf("unknown kind: %s", Kind('?'))
	}
}

func TestAppendCommand(t *testing.T) {
	got := string(AppendCommand(nil, "AUTH", "user", "pa ss\r\nword", ""))
	want := "*4\r\n$4\r\nAUTH\r\n$4\r\nuser\r\n$11\r\npa ss\r\nword\r\n$0\r\n\r\n"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	// What the command writer produces, the reader reads back as the same
	// arguments — a password with CRLF in it is still one argument.
	v, err := read(t, got, Limits{})
	if err != nil || len(v.Elems) != 4 || v.Elems[2].Str != "pa ss\r\nword" {
		t.Fatalf("read back: %+v, %v", v, err)
	}
}

func TestAppendValue_Refuses(t *testing.T) {
	for _, v := range []Value{
		{Kind: SimpleString, Str: "a\r\nb"},
		{Kind: Error, Str: "x\n"},
		{Kind: BigNumber, Str: "12x"},
		{Kind: Verbatim, Format: "tx", Str: "a"},
		{Kind: Map, Elems: []Value{{Kind: Null}}},
		{Kind: Set, Null: true},
		{Kind: Array, Elems: []Value{{Kind: SimpleString, Str: "\r"}}},
		{Kind: 'Q'},
	} {
		if _, err := AppendValue(nil, v); err == nil {
			t.Errorf("%+v: want an error", v)
		}
	}
}

// valueGen draws any value AppendValue can encode, nested up to depth.
func valueGen(depth int) *rapid.Generator[Value] {
	line := rapid.StringMatching(`[ -~]{0,20}`) // printable, so no CR/LF
	scalar := rapid.OneOf(
		rapid.Custom(func(t *rapid.T) Value { return Value{Kind: SimpleString, Str: line.Draw(t, "s")} }),
		rapid.Custom(func(t *rapid.T) Value { return Value{Kind: Error, Str: line.Draw(t, "e")} }),
		rapid.Custom(func(t *rapid.T) Value { return Value{Kind: Integer, Int: rapid.Int64().Draw(t, "i")} }),
		rapid.Custom(func(t *rapid.T) Value { return Value{Kind: BulkString, Str: rapid.String().Draw(t, "b")} }),
		rapid.Just(Value{Kind: BulkString, Null: true}),
		rapid.Just(Value{Kind: Null}),
		rapid.Custom(func(t *rapid.T) Value { return Value{Kind: Boolean, Bool: rapid.Bool().Draw(t, "t")} }),
		rapid.Custom(func(t *rapid.T) Value {
			f := rapid.Float64().Draw(t, "f")
			return Value{Kind: Double, Float: f}
		}),
		rapid.Custom(func(t *rapid.T) Value {
			return Value{Kind: BigNumber, Str: rapid.StringMatching(`-?[0-9]{1,40}`).Draw(t, "n")}
		}),
		rapid.Custom(func(t *rapid.T) Value {
			return Value{Kind: Verbatim, Format: rapid.StringMatching(`[a-z]{3}`).Draw(t, "fmt"), Str: rapid.String().Draw(t, "v")}
		}),
		rapid.Custom(func(t *rapid.T) Value { return Value{Kind: BlobError, Str: rapid.String().Draw(t, "be")} }),
	)
	if depth == 0 {
		return scalar
	}
	inner := valueGen(depth - 1)
	return rapid.OneOf(scalar, rapid.Custom(func(t *rapid.T) Value {
		kind := rapid.SampledFrom([]Kind{Array, Set, Push, Map}).Draw(t, "kind")
		elems := rapid.SliceOfN(inner, 0, 5).Draw(t, "elems")
		if kind == Map && len(elems)%2 == 1 {
			elems = elems[:len(elems)-1]
		}
		if elems == nil {
			elems = []Value{}
		}
		return Value{Kind: kind, Elems: elems}
	}), rapid.Just(Value{Kind: Array, Null: true}))
}

// equal is DeepEqual with NaN equal to NaN, which is what "the same value
// came back" means for a double.
func equal(a, b Value) bool {
	if a.Kind == Double && b.Kind == Double && math.IsNaN(a.Float) && math.IsNaN(b.Float) {
		a.Float, b.Float = 0, 0
	}
	if len(a.Elems) != len(b.Elems) {
		return false
	}
	for i := range a.Elems {
		if !equal(a.Elems[i], b.Elems[i]) {
			return false
		}
	}
	a.Elems, b.Elems = nil, nil
	return reflect.DeepEqual(a, b)
}

func TestProperty_WriteThenReadIsIdentity(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		v := valueGen(3).Draw(t, "v")
		enc, err := AppendValue(nil, v)
		if err != nil {
			t.Fatalf("encode %+v: %v", v, err)
		}
		// Two in a row, so the reader must stop exactly at the end of each.
		r := NewReader(bytes.NewReader(append(enc, enc...)), Limits{})
		for i := range 2 {
			got, err := r.Read()
			if err != nil {
				t.Fatalf("read %d of %q: %v", i, enc, err)
			}
			if !equal(got, v) {
				t.Fatalf("read %d:\n got %+v\nwant %+v", i, got, v)
			}
		}
	})
}

func FuzzRead(f *testing.F) {
	for _, s := range []string{
		"+OK\r\n", "-ERR x\r\n", ":1\r\n", "$3\r\nabc\r\n", "$-1\r\n", "*2\r\n:1\r\n$1\r\nx\r\n",
		"*-1\r\n", "_\r\n", "#t\r\n", ",1.5\r\n", "(12\r\n", "=7\r\ntxt:abc\r\n", "!3\r\nERR\r\n",
		"%1\r\n+k\r\n+v\r\n", "~1\r\n:1\r\n", ">1\r\n+p\r\n", "*1\r\n*1\r\n*1\r\n:1\r\n",
		"$9999999999999999999\r\n", "*9999999999999999999\r\n", "%9223372036854775807\r\n",
	} {
		f.Add([]byte(s))
	}
	lim := Limits{MaxBulk: 64, MaxElems: 8, MaxDepth: 4, MaxTotal: 64 * valueCost}
	f.Fuzz(func(t *testing.T, in []byte) {
		v, err := NewReader(bytes.NewReader(in), lim).Read()
		if err != nil {
			return
		}
		// Whatever was accepted re-encodes, and reads back as itself.
		enc, err := AppendValue(nil, v)
		if err != nil {
			t.Fatalf("accepted %q but cannot encode %+v: %v", in, v, err)
		}
		got, err := NewReader(bytes.NewReader(enc), lim).Read()
		if err != nil || !equal(got, v) {
			t.Fatalf("round trip of %q via %q: got %+v, %v; want %+v", in, enc, got, err, v)
		}
	})
}

// Review finding: a 19-digit length overflowed int64, wrapped negative,
// passed the limit check and panicked in make — one reply from whatever
// answers a check took the agent down. Every kind refuses it as too large.
func TestRead_AnOverflowingLengthIsRefused(t *testing.T) {
	lim := Limits{MaxBulk: 1 << 20, MaxElems: 1 << 10, MaxDepth: 4, MaxTotal: 1 << 24}
	for _, in := range []string{
		"$9999999999999999999\r\n", "*9999999999999999999\r\n", "%9999999999999999999\r\n",
		"~9999999999999999999\r\n", "=9999999999999999999\r\n", "$9223372036854775808\r\n",
	} {
		_, err := NewReader(strings.NewReader(in), lim).Read()
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("%q: err = %v", in, err)
		}
	}
}
