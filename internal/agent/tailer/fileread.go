package tailer

import (
	"bytes"
	"io"
	"os"
	"time"
)

// Line limits.
const (
	// MaxLineBytes is the longest line kept; the rest of a longer line is
	// discarded and the line flagged truncated.
	MaxLineBytes = 256 << 10
	// PartialFlushAfter is how long an unterminated last line waits for its
	// newline before it is emitted as it is.
	PartialFlushAfter = 5 * time.Second
	// readChunk is one read; maxPollBytes bounds what one poll reads from one
	// file, so a huge backlog is worked through in slices and the other files
	// and the shutdown signal still get their turn.
	readChunk    = 64 << 10
	maxPollBytes = 1 << 20
)

// rawLine is one line as read, with the offset just past its newline.
type rawLine struct {
	text      string
	end       int64
	truncated bool
}

// lineReader reads lines from a file at a byte offset it owns. Reading is by
// position (ReadAt), not by the file's cursor, so rewinding after a failed
// delivery is just setting the offset.
type lineReader struct {
	f       *os.File
	offset  int64 // next byte to read
	partial []byte
	since   time.Time // when partial's first byte was read
	// discarding is true while skipping the rest of an over-long line.
	discarding bool
	buf        []byte
}

// seek repositions the reader and drops any half-read line.
func (r *lineReader) seek(off int64) {
	r.offset = off
	r.partial = r.partial[:0]
	r.discarding = false
}

// read returns the complete lines available now, up to maxPollBytes of file.
// truncated reports that the file is shorter than the offset (it was
// truncated in place) and the reader has restarted at 0: the caller counts it.
func (r *lineReader) read(now time.Time) (lines []rawLine, truncated bool, err error) {
	fi, err := r.f.Stat()
	if err != nil {
		return nil, false, err
	}
	if fi.Size() < r.offset {
		r.seek(0)
		truncated = true
	}
	if r.buf == nil {
		r.buf = make([]byte, readChunk)
	}
	budget := maxPollBytes
	for budget > 0 && r.offset < fi.Size() {
		n, rerr := r.f.ReadAt(r.buf, r.offset)
		if n > 0 {
			if len(r.partial) == 0 {
				r.since = now
			}
			lines = r.split(r.buf[:n], lines)
			r.offset += int64(n)
			budget -= n
		}
		if rerr != nil && rerr != io.EOF {
			return lines, truncated, rerr
		}
		if n == 0 {
			break
		}
	}
	// An unterminated last line that has sat for PartialFlushAfter is emitted as
	// it is: a program that exits without a newline must not hide its last line.
	if len(r.partial) > 0 && !r.discarding && now.Sub(r.since) >= PartialFlushAfter {
		lines = append(lines, rawLine{text: trimCR(r.partial), end: r.offset})
		r.partial = r.partial[:0]
	}
	return lines, truncated, nil
}

// split appends the complete lines in chunk to out, keeping the tail in partial.
// chunk begins at r.offset.
func (r *lineReader) split(chunk []byte, out []rawLine) []rawLine {
	pos := r.offset
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			if r.discarding {
				return out
			}
			r.partial = append(r.partial, chunk...)
			if len(r.partial) > MaxLineBytes {
				// Over the limit with no newline in sight: emit what fits now and
				// skip the rest of the line, so memory stays bounded. The offset
				// stops where reading did; a crash here re-reads the tail as a line.
				out = append(out, rawLine{text: trimCR(r.partial[:MaxLineBytes]), end: pos + int64(len(chunk)), truncated: true})
				r.partial = r.partial[:0]
				r.discarding = true
			}
			return out
		}
		pos += int64(i) + 1
		piece := chunk[:i]
		chunk = chunk[i+1:]
		if r.discarding {
			r.discarding = false // the newline ends the over-long line already emitted
			continue
		}
		line := append(r.partial, piece...) //nolint:gocritic // partial is reused deliberately
		r.partial = r.partial[:0]
		trunc := false
		if len(line) > MaxLineBytes {
			line, trunc = line[:MaxLineBytes], true
		}
		out = append(out, rawLine{text: trimCR(line), end: pos, truncated: trunc})
	}
	return out
}

func trimCR(b []byte) string { return string(bytes.TrimSuffix(b, []byte{'\r'})) }
