package resp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unsafe"
)

// Kind is a RESP type, named by its type byte.
type Kind byte

// The RESP2 kinds, then the RESP3 ones the reader also accepts.
const (
	SimpleString Kind = '+'
	Error        Kind = '-'
	Integer      Kind = ':'
	BulkString   Kind = '$'
	Array        Kind = '*'

	Null      Kind = '_'
	Boolean   Kind = '#'
	Double    Kind = ','
	BigNumber Kind = '('
	Verbatim  Kind = '='
	BlobError Kind = '!'
	Map       Kind = '%'
	Set       Kind = '~'
	Push      Kind = '>'
)

func (k Kind) String() string {
	switch k {
	case SimpleString:
		return "simple string"
	case Error:
		return "error"
	case Integer:
		return "integer"
	case BulkString:
		return "bulk string"
	case Array:
		return "array"
	case Null:
		return "null"
	case Boolean:
		return "boolean"
	case Double:
		return "double"
	case BigNumber:
		return "big number"
	case Verbatim:
		return "verbatim string"
	case BlobError:
		return "blob error"
	case Map:
		return "map"
	case Set:
		return "set"
	case Push:
		return "push"
	}
	return fmt.Sprintf("kind(%q)", byte(k))
}

// Value is one decoded RESP value. Only the fields its Kind uses are set.
//
// One struct rather than an interface per type: a reply is small and short
// lived, and a flat struct makes "is this the answer I expected" a field
// comparison instead of a type switch at every call site.
type Value struct {
	Kind Kind
	// Str holds a simple string, bulk string, verbatim string (without its
	// format prefix), big number (as its decimal text), or an error's text.
	Str string
	// Format is a verbatim string's three-byte format, e.g. "txt".
	Format string
	Int    int64   // Integer
	Float  float64 // Double
	Bool   bool    // Boolean
	// Null marks a null bulk string or null array ($-1, *-1); a RESP3 null
	// has Kind Null instead.
	Null bool
	// Elems holds an array's, set's or push's elements; a map's are flattened
	// as key, value, key, value.
	Elems []Value
}

// Err returns the value as an error if it is one (Error or BlobError), else
// nil.
func (v Value) Err() error {
	if v.Kind == Error || v.Kind == BlobError {
		return &ServerError{Msg: v.Str}
	}
	return nil
}

// Text returns the value's string content for the kinds that carry one
// (simple, bulk, verbatim), and whether it had one. A null bulk string has
// none.
func (v Value) Text() (string, bool) {
	switch v.Kind {
	case SimpleString, Verbatim:
		return v.Str, true
	case BulkString:
		return v.Str, !v.Null
	}
	return "", false
}

// ServerError is an error reply: the server understood the request and said
// no. It is not a protocol error, and the connection stays usable.
type ServerError struct {
	Msg string // e.g. "WRONGPASS invalid username-password pair"
}

func (e *ServerError) Error() string { return "redis: " + e.Msg }

// Code returns the error's first word, which Redis uses as a machine-readable
// code: ERR, WRONGPASS, NOAUTH, WRONGTYPE, …
func (e *ServerError) Code() string {
	code, _, _ := strings.Cut(e.Msg, " ")
	return code
}

// ErrProtocol wraps every error that means the bytes were not valid RESP. The
// stream cannot be trusted after one: the reader no longer knows where the
// next reply starts.
var ErrProtocol = errors.New("resp: protocol error")

// ErrTooLarge wraps every error that means a reply exceeded [Limits]. Like a
// protocol error it leaves the stream in an unknown place.
var ErrTooLarge = errors.New("resp: reply exceeds limits")

// Limits bound what one reply may cost. Zero fields take the defaults in
// [DefaultLimits].
type Limits struct {
	// MaxBulk bounds one bulk, verbatim or blob-error string, and the
	// content of one line (after its type byte).
	MaxBulk int
	// MaxElems bounds one aggregate's announced element count (a map's
	// count is of pairs, so it may hold twice this many values).
	MaxElems int
	// MaxDepth bounds aggregate nesting; a top-level array is depth 1.
	MaxDepth int
	// MaxTotal bounds the memory one reply may cost: string bytes plus a
	// fixed charge per decoded value.
	MaxTotal int64
}

// DefaultLimits suit INFO, which is a few kilobytes even on a large server
// ("INFO everything" with many clients and databases stays well under 1 MiB).
// They are generous for INFO and hostile to everything else, which is the
// intent: this reader is not a general-purpose Redis client.
var DefaultLimits = Limits{MaxBulk: 4 << 20, MaxElems: 1 << 16, MaxDepth: 16, MaxTotal: 8 << 20}

func (l Limits) withDefaults() Limits {
	if l.MaxBulk <= 0 {
		l.MaxBulk = DefaultLimits.MaxBulk
	}
	if l.MaxElems <= 0 {
		l.MaxElems = DefaultLimits.MaxElems
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = DefaultLimits.MaxDepth
	}
	if l.MaxTotal <= 0 {
		l.MaxTotal = DefaultLimits.MaxTotal
	}
	return l
}

// valueCost is what every decoded value is charged against Limits.MaxTotal
// on top of its string bytes, so a reply of a million empty elements is as
// bounded as one of a million bytes.
const valueCost = int64(unsafe.Sizeof(Value{}))

// Reader reads RESP values from a stream.
type Reader struct {
	br     *bufio.Reader
	lim    Limits
	budget int64 // what the reply being read may still cost
	line   []byte
}

// NewReader returns a Reader over r with the given limits.
func NewReader(r io.Reader, lim Limits) *Reader {
	return &Reader{br: bufio.NewReader(r), lim: lim.withDefaults()}
}

// Read reads one complete reply. An error reply is returned as a Value of
// Kind Error with a nil error (it is a valid reply; see [Value.Err]). A
// non-nil error is an I/O error, [ErrProtocol] or [ErrTooLarge], after which
// the stream is unusable.
func (r *Reader) Read() (Value, error) {
	r.budget = r.lim.MaxTotal
	return r.read(0)
}

func (r *Reader) charge(n int64) error {
	r.budget -= n
	if r.budget < 0 {
		return fmt.Errorf("%w: reply costs more than %d bytes", ErrTooLarge, r.lim.MaxTotal)
	}
	return nil
}

func (r *Reader) read(depth int) (Value, error) {
	if err := r.charge(valueCost); err != nil {
		return Value{}, err
	}
	line, err := r.readLine()
	if err != nil {
		return Value{}, err
	}
	if len(line) == 0 {
		return Value{}, fmt.Errorf("%w: empty line where a type byte belongs", ErrProtocol)
	}
	kind, rest := Kind(line[0]), line[1:]
	switch kind {
	case SimpleString, Error, BigNumber:
		if kind == BigNumber && !isBigNumber(rest) {
			return Value{}, fmt.Errorf("%w: big number %q", ErrProtocol, rest)
		}
		if err := r.charge(int64(len(rest))); err != nil {
			return Value{}, err
		}
		return Value{Kind: kind, Str: string(rest)}, nil
	case Integer:
		n, err := strconv.ParseInt(string(rest), 10, 64)
		if err != nil {
			return Value{}, fmt.Errorf("%w: integer %q", ErrProtocol, rest)
		}
		return Value{Kind: Integer, Int: n}, nil
	case Null:
		if len(rest) != 0 {
			return Value{}, fmt.Errorf("%w: null with content %q", ErrProtocol, rest)
		}
		return Value{Kind: Null}, nil
	case Boolean:
		switch string(rest) {
		case "t":
			return Value{Kind: Boolean, Bool: true}, nil
		case "f":
			return Value{Kind: Boolean}, nil
		}
		return Value{}, fmt.Errorf("%w: boolean %q", ErrProtocol, rest)
	case Double:
		f, err := parseDouble(string(rest))
		if err != nil {
			return Value{}, fmt.Errorf("%w: double %q", ErrProtocol, rest)
		}
		return Value{Kind: Double, Float: f}, nil
	case BulkString, Verbatim, BlobError:
		return r.readBlob(kind, rest)
	case Array, Map, Set, Push:
		return r.readAggregate(kind, rest, depth)
	}
	return Value{}, fmt.Errorf("%w: unknown type byte %q", ErrProtocol, line[0])
}

// readLine returns the next line without its CRLF. The slice is only valid
// until the next read. A line is bounded by MaxBulk: lines have no length
// prefix, so a stream that never sends CRLF is refused by scanning, not by
// waiting forever for memory to run out.
func (r *Reader) readLine() ([]byte, error) {
	r.line = r.line[:0]
	for {
		chunk, err := r.br.ReadSlice('\n')
		// +3: the type byte and the CRLF are not content.
		if len(r.line)+len(chunk) > r.lim.MaxBulk+3 {
			return nil, fmt.Errorf("%w: line longer than %d bytes", ErrTooLarge, r.lim.MaxBulk)
		}
		switch {
		case err == nil:
			if len(r.line) == 0 {
				// The common case: the whole line was in the buffer; no copy.
				return trimCRLF(chunk)
			}
			r.line = append(r.line, chunk...)
			return trimCRLF(r.line)
		case errors.Is(err, bufio.ErrBufferFull):
			r.line = append(r.line, chunk...)
		case errors.Is(err, io.EOF) && len(r.line)+len(chunk) > 0:
			return nil, fmt.Errorf("%w: stream ended mid-line", io.ErrUnexpectedEOF)
		default:
			return nil, err
		}
	}
}

func trimCRLF(b []byte) ([]byte, error) {
	if len(b) < 2 || b[len(b)-2] != '\r' {
		return nil, fmt.Errorf("%w: line not terminated by CRLF", ErrProtocol)
	}
	// A line cannot carry a CR of its own (the fuzzer's first find: "+\r\r\n"
	// read as a simple string holding "\r", which RESP cannot express).
	if bytes.IndexByte(b[:len(b)-2], '\r') >= 0 {
		return nil, fmt.Errorf("%w: CR inside a line", ErrProtocol)
	}
	return b[:len(b)-2], nil
}

// readLength parses an aggregate count or blob length: -1 means null, and
// anything else must be a plain non-negative decimal no greater than max.
func readLength(rest []byte, max int, what string) (n int, null bool, err error) {
	if string(rest) == "-1" {
		return 0, true, nil
	}
	if len(rest) == 0 || len(rest) > 19 {
		return 0, false, fmt.Errorf("%w: %s length %q", ErrProtocol, what, rest)
	}
	// Past max the value is refused, so accumulating stops there: carried
	// on, 19 digits overflow int64, wrap negative, and pass a v > max test
	// — and a negative length reaches make([]byte, n) and panics, taking
	// the whole agent with it on the word of whatever answers the check.
	var v int64
	over := false
	for _, c := range rest {
		if c < '0' || c > '9' {
			return 0, false, fmt.Errorf("%w: %s length %q", ErrProtocol, what, rest)
		}
		if !over {
			v = v*10 + int64(c-'0')
			over = v > int64(max)
		}
	}
	if over {
		return 0, false, fmt.Errorf("%w: %s length %s exceeds %d", ErrTooLarge, what, rest, max)
	}
	return int(v), false, nil
}

func (r *Reader) readBlob(kind Kind, rest []byte) (Value, error) {
	n, null, err := readLength(rest, r.lim.MaxBulk, kind.String())
	if err != nil {
		return Value{}, err
	}
	if null {
		if kind != BulkString {
			return Value{}, fmt.Errorf("%w: null %s", ErrProtocol, kind)
		}
		return Value{Kind: BulkString, Null: true}, nil
	}
	// Charged before the allocation: a length the budget cannot pay for is
	// refused without making the buffer.
	if err := r.charge(int64(n)); err != nil {
		return Value{}, err
	}
	buf := make([]byte, n+2)
	if _, err := io.ReadFull(r.br, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return Value{}, fmt.Errorf("reading a %d-byte %s: %w", n, kind, err)
	}
	if buf[n] != '\r' || buf[n+1] != '\n' {
		return Value{}, fmt.Errorf("%w: %s not terminated by CRLF after %d bytes", ErrProtocol, kind, n)
	}
	s := string(buf[:n])
	switch kind {
	case Verbatim:
		// "txt:" then the content: a three-byte format and a colon.
		if len(s) < 4 || s[3] != ':' {
			return Value{}, fmt.Errorf("%w: verbatim string without a format prefix", ErrProtocol)
		}
		return Value{Kind: Verbatim, Format: s[:3], Str: s[4:]}, nil
	case BlobError:
		return Value{Kind: BlobError, Str: s}, nil
	}
	return Value{Kind: BulkString, Str: s}, nil
}

func (r *Reader) readAggregate(kind Kind, rest []byte, depth int) (Value, error) {
	if depth+1 > r.lim.MaxDepth {
		return Value{}, fmt.Errorf("%w: nesting deeper than %d", ErrTooLarge, r.lim.MaxDepth)
	}
	n, null, err := readLength(rest, r.lim.MaxElems, kind.String())
	if err != nil {
		return Value{}, err
	}
	if null {
		if kind != Array {
			return Value{}, fmt.Errorf("%w: null %s", ErrProtocol, kind)
		}
		return Value{Kind: Array, Null: true}, nil
	}
	count := n
	if kind == Map {
		count = 2 * n
	}
	// Grown as elements arrive, not sized to the claim: "*65536" followed by
	// nothing must not cost 65536 slots.
	elems := make([]Value, 0, min(count, 16))
	for range count {
		v, err := r.read(depth + 1)
		if err != nil {
			return Value{}, err
		}
		elems = append(elems, v)
	}
	return Value{Kind: kind, Elems: elems}, nil
}

func isBigNumber(b []byte) bool {
	if len(b) > 0 && (b[0] == '-' || b[0] == '+') {
		b = b[1:]
	}
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// parseDouble accepts RESP3's spellings: decimal or exponent notation, "inf",
// "-inf" and "nan". strconv also accepts hex floats, underscores and
// "Infinity", which RESP does not, so those are refused first.
func parseDouble(s string) (float64, error) {
	switch s {
	case "inf", "+inf":
		return math.Inf(1), nil
	case "-inf":
		return math.Inf(-1), nil
	case "nan", "-nan":
		return math.NaN(), nil
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && !strings.ContainsRune(".eE+-", rune(c)) {
			return 0, strconv.ErrSyntax
		}
	}
	return strconv.ParseFloat(s, 64)
}

// AppendCommand appends a command — an array of bulk strings — to dst. Every
// argument is binary safe: a password containing CRLF or a space is sent as
// one argument, which is why commands are never written as inline text.
func AppendCommand(dst []byte, args ...string) []byte {
	dst = append(dst, '*')
	dst = strconv.AppendInt(dst, int64(len(args)), 10)
	dst = append(dst, '\r', '\n')
	for _, a := range args {
		dst = appendBlob(dst, '$', a)
	}
	return dst
}

func appendBlob(dst []byte, kind Kind, s string) []byte {
	dst = append(dst, byte(kind))
	dst = strconv.AppendInt(dst, int64(len(s)), 10)
	dst = append(dst, '\r', '\n')
	dst = append(dst, s...)
	return append(dst, '\r', '\n')
}

// AppendValue appends v's RESP encoding to dst. The check itself only sends
// commands; this is the other half of the protocol, for fake servers in tests
// and for the round-trip property the reader is tested against. It fails for
// values RESP cannot encode as given: a line kind (simple string, error, big
// number) containing CR or LF, a verbatim format that is not three bytes,
// a map with an odd number of elements, or an unknown kind.
func AppendValue(dst []byte, v Value) ([]byte, error) {
	switch v.Kind {
	case SimpleString, Error, BigNumber:
		if strings.ContainsAny(v.Str, "\r\n") {
			return dst, fmt.Errorf("resp: a %s cannot contain CR or LF", v.Kind)
		}
		if v.Kind == BigNumber && !isBigNumber([]byte(v.Str)) {
			return dst, fmt.Errorf("resp: big number %q is not an integer", v.Str)
		}
		dst = append(dst, byte(v.Kind))
		dst = append(dst, v.Str...)
		return append(dst, '\r', '\n'), nil
	case Integer:
		dst = append(dst, ':')
		dst = strconv.AppendInt(dst, v.Int, 10)
		return append(dst, '\r', '\n'), nil
	case Null:
		return append(dst, '_', '\r', '\n'), nil
	case Boolean:
		if v.Bool {
			return append(dst, "#t\r\n"...), nil
		}
		return append(dst, "#f\r\n"...), nil
	case Double:
		dst = append(dst, ',')
		switch {
		case math.IsInf(v.Float, 1):
			dst = append(dst, "inf"...)
		case math.IsInf(v.Float, -1):
			dst = append(dst, "-inf"...)
		case math.IsNaN(v.Float):
			dst = append(dst, "nan"...)
		default:
			dst = strconv.AppendFloat(dst, v.Float, 'g', -1, 64)
		}
		return append(dst, '\r', '\n'), nil
	case BulkString:
		if v.Null {
			return append(dst, "$-1\r\n"...), nil
		}
		return appendBlob(dst, BulkString, v.Str), nil
	case BlobError:
		return appendBlob(dst, BlobError, v.Str), nil
	case Verbatim:
		if len(v.Format) != 3 {
			return dst, fmt.Errorf("resp: verbatim format %q is not three bytes", v.Format)
		}
		return appendBlob(dst, Verbatim, v.Format+":"+v.Str), nil
	case Array, Map, Set, Push:
		if v.Null {
			if v.Kind != Array {
				return dst, fmt.Errorf("resp: a %s cannot be null", v.Kind)
			}
			return append(dst, "*-1\r\n"...), nil
		}
		n := len(v.Elems)
		if v.Kind == Map {
			if n%2 != 0 {
				return dst, errors.New("resp: a map needs an even number of elements")
			}
			n /= 2
		}
		dst = append(dst, byte(v.Kind))
		dst = strconv.AppendInt(dst, int64(n), 10)
		dst = append(dst, '\r', '\n')
		for _, e := range v.Elems {
			var err error
			if dst, err = AppendValue(dst, e); err != nil {
				return dst, err
			}
		}
		return dst, nil
	}
	return dst, fmt.Errorf("resp: cannot encode %s", v.Kind)
}
