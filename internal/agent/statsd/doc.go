// Package statsd is the agent's extended StatsD server: a UDP listener on :8125 and
// a parser for the datagram format in docs/wire-protocol.md §A.
//
// Why UDP: an app must never block, slow down or fail because its metrics
// can't be delivered. A UDP send is a syscall that returns immediately
// whether or not anyone is listening. The price is that datagrams can be
// lost — by the kernel when a socket buffer is full, silently — so the
// server counts everything it can see (received, dropped from its own queue,
// malformed) and the operator compares that with what clients sent.
//
// The shape of the server is the classic one for a lossy, bursty input:
//
//	socket ──readers──▶ bounded queue ──workers──▶ Parse ──▶ Sink
//
// Readers only copy datagrams off the socket into pooled buffers, so the
// kernel buffer drains as fast as possible; parsing happens on workers. When
// the queue is full a reader drops the datagram and counts it rather than
// wait, because a waiting reader just moves the loss into the kernel, where
// nobody can count it.
//
// Parse allocates nothing: a Message's fields are slices of the input line.
// That makes the hot path cheap (tens of nanoseconds a line) and puts the
// cost of copying on the one place that must keep data — the aggregator,
// which copies a name and tags only when it meets a new series.
package statsd
