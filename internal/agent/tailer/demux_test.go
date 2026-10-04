package tailer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func frame(stream byte, payload string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(payload)))
	return append(h, payload...)
}

func drain(t *testing.T, d *demuxer) ([]dockerLine, error) {
	t.Helper()
	var all []dockerLine
	for {
		lines, err := d.next()
		all = append(all, lines...)
		if err != nil {
			return all, err
		}
	}
}

const ts1 = "2026-10-04T12:00:00.123456789Z"

func TestDemux_FramesSplitLinesAndStreamsInterleave(t *testing.T) {
	var b bytes.Buffer
	// One line split over three frames, with stderr's line arriving in between.
	b.Write(frame(1, ts1+" hel"))
	b.Write(frame(2, "2026-10-04T12:00:00.5Z oops\n"))
	b.Write(frame(1, "lo wo"))
	b.Write(frame(1, "rld\n2026-10-04T12:00:01Z second\n"))
	lines, err := drain(t, newDemuxer(&b, false))
	if !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("%+v", lines)
	}
	if lines[0].text != "oops" || !lines[0].stderr {
		t.Fatalf("%+v", lines[0])
	}
	if lines[1].text != "hello world" || lines[1].stderr || lines[1].ts != 1791115200123456789 {
		t.Fatalf("%+v", lines[1])
	}
	if lines[2].text != "second" {
		t.Fatalf("%+v", lines[2])
	}
}

func TestDemux_ReadsOneByteAtATime(t *testing.T) {
	var b bytes.Buffer
	b.Write(frame(1, ts1+" a\n"))
	b.Write(frame(2, ts1+" b\n"))
	lines, err := drain(t, newDemuxer(iotest.OneByteReader(&b), false))
	if !errors.Is(err, io.EOF) || len(lines) != 2 || lines[0].text != "a" || lines[1].text != "b" {
		t.Fatalf("%v %+v", err, lines)
	}
}

func TestDemux_AnUnterminatedTailIsReturnedAtEOF(t *testing.T) {
	lines, err := drain(t, newDemuxer(bytes.NewReader(frame(1, ts1+" last words")), false))
	if !errors.Is(err, io.EOF) || len(lines) != 1 || lines[0].text != "last words" {
		t.Fatalf("%v %+v", err, lines)
	}
}

func TestDemux_TTYStreamsAreRawText(t *testing.T) {
	in := ts1 + " one\r\n2026-10-04T12:00:01Z two\n" + "2026-10-04T12:00:02Z three"
	lines, err := drain(t, newDemuxer(strings.NewReader(in), true))
	if !errors.Is(err, io.EOF) || len(lines) != 3 || lines[0].text != "one" || lines[2].text != "three" {
		t.Fatalf("%v %+v", err, lines)
	}
}

func TestDemux_AMalformedStreamIsAnErrorNotAGuess(t *testing.T) {
	for name, in := range map[string][]byte{
		"unknown stream":     append([]byte{9, 0, 0, 0, 0, 0, 0, 1}, 'x'),
		"nonzero padding":    {1, 0, 7, 0, 0, 0, 0, 1, 'x'},
		"absurd size":        {1, 0, 0, 0, 0xff, 0xff, 0xff, 0xff},
		"raw text as frames": []byte("2026-10-04T12:00:00Z a line from a TTY container\n"),
	} {
		_, err := drain(t, newDemuxer(bytes.NewReader(in), false))
		if !errors.Is(err, ErrBadFrame) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A stream cut inside a frame is an error to retry, not a clean end.
	for name, in := range map[string][]byte{
		"inside header":  {1, 0, 0},
		"inside payload": frame(1, "abcdef")[:11],
	} {
		_, err := drain(t, newDemuxer(bytes.NewReader(in), false))
		if err == nil || errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestDemux_LinesWithoutATimestampKeepTheLastOne(t *testing.T) {
	in := frame(1, ts1+" stamped\nno stamp here\n")
	lines, _ := drain(t, newDemuxer(bytes.NewReader(in), false))
	if len(lines) != 2 || lines[1].text != "no stamp here" || lines[1].ts != lines[0].ts {
		t.Fatalf("%+v", lines)
	}
	// A first word that merely looks like one is not stripped.
	lines, _ = drain(t, newDemuxer(bytes.NewReader(frame(1, "not-a-time at all\n")), false))
	if lines[0].text != "not-a-time at all" || lines[0].ts != 0 {
		t.Fatalf("%+v", lines[0])
	}
}

func TestDemux_ALongLineIsCappedAcrossFrames(t *testing.T) {
	var b bytes.Buffer
	b.Write(frame(1, ts1+" "))
	chunk := strings.Repeat("x", 16<<10)
	for i := 0; i < (MaxLineBytes/len(chunk))*3; i++ {
		b.Write(frame(1, chunk))
	}
	b.Write(frame(1, "\nafter\n"))
	lines, _ := drain(t, newDemuxer(&b, false))
	if len(lines) != 2 || len(lines[0].text) != MaxLineBytes || !lines[0].truncated || lines[1].text != "after" {
		t.Fatalf("%d lines; first %d bytes truncated=%v", len(lines), len(lines[0].text), lines[0].truncated)
	}
}

func TestAssembler_BoundsAnUnterminatedLineWithoutWaitingForItsEnd(t *testing.T) {
	a := lineAssembler{}
	var lines []dockerLine
	for i := 0; i < 8; i++ {
		lines = a.write([]byte(strings.Repeat("q", MaxLineBytes/2)), lines)
	}
	if len(lines) != 1 || !lines[0].truncated || len(a.buf) > MaxLineBytes {
		t.Fatalf("%d lines emitted, %d bytes buffered: memory is not bounded by the line cap", len(lines), len(a.buf))
	}
}
