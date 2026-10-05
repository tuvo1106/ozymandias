package tailer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Docker's log stream, when the container has no terminal, is a sequence of
// frames: an 8-byte header [stream, 0, 0, 0, size uint32 big-endian] and then
// size bytes of payload. stream is 1 for stdout and 2 for stderr. A frame is
// not a line: a long line is split across frames, and one frame can hold
// several lines, so lines are reassembled per stream (stdout and stderr
// interleave, and a half line of one must not be joined to the other).
//
// A container started with a TTY (`docker run -t`) streams raw text instead,
// with stderr already merged into stdout by the terminal.

// maxFrameBytes bounds one frame's payload. The daemon writes frames of at
// most 16 KiB; a header claiming more is a corrupt stream (or a TTY container
// read as framed), and trusting it would allocate whatever the header says.
const maxFrameBytes = 1 << 20

// ErrBadFrame is a frame header that is not one: the stream is out of sync
// and the only safe action is to reconnect.
var ErrBadFrame = errors.New("tailer: malformed docker log frame")

const (
	streamStdout = 1
	streamStderr = 2
)

// dockerLine is one assembled line with its daemon timestamp.
type dockerLine struct {
	ts        int64 // unix nanoseconds; 0 if the line carried none
	text      string
	stderr    bool
	truncated bool
}

// lineAssembler turns byte chunks into lines for one stream.
type lineAssembler struct {
	buf        []byte
	discarding bool
	stderr     bool
	// last is the timestamp of the previous line, used when a line has none.
	last int64
}

// write adds a chunk and returns the lines it completed.
func (a *lineAssembler) write(chunk []byte, out []dockerLine) []dockerLine {
	for len(chunk) > 0 {
		i := indexNL(chunk)
		if i < 0 {
			if a.discarding {
				return out
			}
			a.buf = append(a.buf, chunk...)
			if len(a.buf) > MaxLineBytes+64 { // 64: room for the timestamp prefix
				out = append(out, a.line(a.buf, true))
				a.buf = a.buf[:0]
				a.discarding = true
			}
			return out
		}
		piece := chunk[:i]
		chunk = chunk[i+1:]
		if a.discarding {
			a.discarding = false
			continue
		}
		a.buf = append(a.buf, piece...)
		trunc := len(a.buf) > MaxLineBytes+64
		out = append(out, a.line(a.buf, trunc))
		a.buf = a.buf[:0]
	}
	return out
}

// finish returns the unterminated tail at end of stream, if any.
func (a *lineAssembler) finish(out []dockerLine) []dockerLine {
	if len(a.buf) > 0 && !a.discarding {
		out = append(out, a.line(a.buf, false))
	}
	a.buf = a.buf[:0]
	return out
}

func indexNL(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}

// line splits "2026-10-04T12:00:00.123456789Z message" into timestamp and text.
func (a *lineAssembler) line(raw []byte, truncated bool) dockerLine {
	s := strings.TrimSuffix(string(raw), "\r")
	l := dockerLine{stderr: a.stderr, truncated: truncated, ts: a.last, text: s}
	if sp := strings.IndexByte(s, ' '); sp > 0 {
		if t, err := time.Parse(time.RFC3339Nano, s[:sp]); err == nil {
			l.ts, l.text = t.UnixNano(), s[sp+1:]
			a.last = l.ts
		}
	}
	if len(l.text) > MaxLineBytes {
		l.text, l.truncated = l.text[:MaxLineBytes], true
	}
	return l
}

// demuxer reads a docker log stream and yields complete lines.
type demuxer struct {
	r      io.Reader
	tty    bool
	hdr    [8]byte
	out, e lineAssembler
	buf    []byte
}

func newDemuxer(r io.Reader, tty bool) *demuxer {
	return &demuxer{r: r, tty: tty, e: lineAssembler{stderr: true}}
}

// next reads until at least one line is complete, the stream ends (io.EOF,
// with any unterminated tail returned first), or it breaks.
func (d *demuxer) next() ([]dockerLine, error) {
	if d.tty {
		if d.buf == nil {
			d.buf = make([]byte, 32<<10)
		}
		for {
			n, err := d.r.Read(d.buf)
			lines := d.out.write(d.buf[:n], nil)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return d.out.finish(lines), io.EOF
				}
				return lines, err
			}
			if len(lines) > 0 {
				return lines, nil
			}
		}
	}
	for {
		if _, err := io.ReadFull(d.r, d.hdr[:]); err != nil {
			if errors.Is(err, io.EOF) { // clean end between frames
				lines := d.out.finish(nil)
				return d.e.finish(lines), io.EOF
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, fmt.Errorf("tailer: log stream cut inside a frame header: %w", io.ErrUnexpectedEOF)
			}
			return nil, err
		}
		kind := d.hdr[0]
		size := binary.BigEndian.Uint32(d.hdr[4:])
		if kind > streamStderr || d.hdr[1] != 0 || d.hdr[2] != 0 || d.hdr[3] != 0 || size > maxFrameBytes {
			return nil, fmt.Errorf("%w: header % x", ErrBadFrame, d.hdr)
		}
		if cap(d.buf) < int(size) {
			d.buf = make([]byte, size)
		}
		payload := d.buf[:size]
		if _, err := io.ReadFull(d.r, payload); err != nil {
			return nil, fmt.Errorf("tailer: log stream cut inside a frame: %w", err)
		}
		var lines []dockerLine
		switch kind {
		case streamStdout:
			lines = d.out.write(payload, nil)
		case streamStderr:
			lines = d.e.write(payload, nil)
		}
		if len(lines) > 0 {
			return lines, nil
		}
	}
}
