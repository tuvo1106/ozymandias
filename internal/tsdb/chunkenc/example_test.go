package chunkenc_test

import (
	"fmt"

	"github.com/tuvo1106/ozymandias/internal/tsdb/chunkenc"
)

// A chunk is written once, sample by sample, and then read back in order.
// This is the shape of every series in the head.
func ExampleChunk() {
	c := chunkenc.NewChunk()
	a, err := c.Appender()
	if err != nil {
		panic(err)
	}
	// A counter scraped every 10 seconds that is not moving: the timestamps
	// have a constant delta and the value never changes, which is the case
	// Gorilla was designed for.
	for i := 0; i < 120; i++ {
		if err := a.Append(1600000000000+int64(i)*10_000, 42); err != nil {
			panic(err)
		}
	}

	it := c.Iterator()
	it.Next()
	firstT, firstV := it.At()
	var last int64
	n := 1
	for it.Next() {
		last, _ = it.At()
		n++
	}
	if err := it.Err(); err != nil {
		panic(err)
	}

	fmt.Printf("%d samples from %d to %d, first value %g\n", n, firstT, last, firstV)
	fmt.Printf("%d bytes on the wire, %d as (int64, float64) pairs\n", len(c.Bytes()), n*16)
	fmt.Printf("chunk is full: %v\n", c.Full())
	// Output:
	// 120 samples from 1600000000000 to 1600001190000, first value 42
	// 49 bytes on the wire, 1920 as (int64, float64) pairs
	// chunk is full: true
}
