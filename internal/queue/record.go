package queue

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// recordType is the WAL record type every queue record is framed with. A
// different type in a topic directory is a file that is not ours (or a future
// format this build cannot read) and is reported, not skipped.
const recordType uint8 = 1

// recordHeader is offset u64 | time i64 (unix nanoseconds), big-endian, in
// front of the payload inside the WAL record. The WAL's own frame already
// carries the length and a CRC, so this adds no second checksum.
const recordHeader = 8 + 8

// MaxPayload is the largest payload one record can carry: the WAL's record
// limit less the queue's header.
const MaxPayload = wal.MaxRecordSize - recordHeader

// ErrCorrupt is returned when a topic's files hold something the queue could
// not have written: a record of the wrong type or size, or offsets that do not
// run consecutively.
var ErrCorrupt = errors.New("queue: corrupt topic")

// Record is one delivered record.
type Record struct {
	// Offset is the record's position in its topic: consecutive, from 0.
	Offset uint64
	// Time is when the record was produced, by the topic's clock. Consumers
	// use it for lag in seconds; it is not the data's own timestamp.
	Time time.Time
	// Payload is the producer's bytes. It is owned by the caller.
	Payload []byte
}

// putOffset writes off into an encoded record's header.
func putOffset(b []byte, off uint64) { binary.BigEndian.PutUint64(b[0:8], off) }

func encodeRecord(off uint64, at time.Time, payload []byte) []byte {
	b := make([]byte, recordHeader+len(payload))
	binary.BigEndian.PutUint64(b[0:8], off)
	binary.BigEndian.PutUint64(b[8:16], uint64(at.UnixNano()))
	copy(b[recordHeader:], payload)
	return b
}

// recordOffset validates a WAL record as a queue record and returns its
// offset, without copying the payload: recovery needs only the offsets.
func recordOffset(r wal.Record) (uint64, error) {
	if r.Type != recordType {
		return 0, fmt.Errorf("%w: record type %d, want %d", ErrCorrupt, r.Type, recordType)
	}
	if len(r.Data) < recordHeader {
		return 0, fmt.Errorf("%w: record of %d bytes is shorter than its %d-byte header",
			ErrCorrupt, len(r.Data), recordHeader)
	}
	return binary.BigEndian.Uint64(r.Data[0:8]), nil
}

// decodeRecord parses a WAL record into a queue record, copying the payload
// (the WAL reader reuses its buffer).
func decodeRecord(r wal.Record) (Record, error) {
	if _, err := recordOffset(r); err != nil {
		return Record{}, err
	}
	payload := make([]byte, len(r.Data)-recordHeader)
	copy(payload, r.Data[recordHeader:])
	return Record{
		Offset:  binary.BigEndian.Uint64(r.Data[0:8]),
		Time:    time.Unix(0, int64(binary.BigEndian.Uint64(r.Data[8:16]))),
		Payload: payload,
	}, nil
}

// Offset files: next u64 | crc32c(next) u32, big-endian, 12 bytes.
//
// What is stored is the *next offset to read*, not the last one processed,
// so "nothing committed yet" is 0 rather than a sentinel. The checksum is
// there because the file is read at startup to decide where to resume: a
// flipped bit would otherwise silently skip (or replay) a range of records.
const offsetFileSize = 8 + 4

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func encodeOffset(next uint64) []byte {
	b := make([]byte, offsetFileSize)
	binary.BigEndian.PutUint64(b[0:8], next)
	binary.BigEndian.PutUint32(b[8:12], crc32.Checksum(b[0:8], castagnoli))
	return b
}

func decodeOffset(b []byte) (uint64, error) {
	if len(b) != offsetFileSize {
		return 0, fmt.Errorf("%w: offset file of %d bytes, want %d", ErrCorrupt, len(b), offsetFileSize)
	}
	if got, want := crc32.Checksum(b[0:8], castagnoli), binary.BigEndian.Uint32(b[8:12]); got != want {
		return 0, fmt.Errorf("%w: offset file crc %08x, want %08x", ErrCorrupt, got, want)
	}
	return binary.BigEndian.Uint64(b[0:8]), nil
}

// writeOffsetFile replaces path atomically: write a temp file, fsync it,
// rename it over the old one, fsync the directory. A crash at any point leaves
// either the old offset or the new one, never a torn file — the old one means
// some records are redelivered, which consumers tolerate by design.
func writeOffsetFile(path string, next uint64) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("queue: creating %s: %w", tmp, err)
	}
	if _, err := f.Write(encodeOffset(next)); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("queue: writing %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("queue: syncing %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("queue: closing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("queue: publishing %s: %w", path, err)
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("queue: opening dir %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("queue: syncing dir %s: %w", dir, err)
	}
	return nil
}
