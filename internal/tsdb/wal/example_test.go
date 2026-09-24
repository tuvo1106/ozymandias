package wal_test

import (
	"fmt"
	"os"

	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// A log is written, closed, and replayed — which is exactly what a restart
// does. Records are opaque to the log: it frames and checksums them, and the
// type byte is the caller's to interpret.
func ExampleWAL() {
	dir, err := os.MkdirTemp("", "wal-example")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		panic(err)
	}
	// One call, so one fsync covers both records. That is the whole
	// performance story of a write-ahead log.
	if err := w.Log(
		wal.Record{Type: 1, Data: []byte("series 7 = http.request.count{env:prod}")},
		wal.Record{Type: 2, Data: []byte("series 7 @ t=1600000000000 v=42")},
	); err != nil {
		panic(err)
	}
	if err := w.Sync(); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}

	r, err := wal.NewReader(dir)
	if err != nil {
		panic(err)
	}
	defer func() { _ = r.Close() }()
	for r.Next() {
		rec := r.Record()
		fmt.Printf("type %d: %s\n", rec.Type, rec.Data)
	}
	if err := r.Err(); err != nil {
		panic(err)
	}
	// Output:
	// type 1: series 7 = http.request.count{env:prod}
	// type 2: series 7 @ t=1600000000000 v=42
}
