// Package resp speaks just enough of the Redis serialization protocol (RESP)
// for the agent's redis check to ask a server for INFO and read the answer —
// without a Redis client library, because writing the protocol is the point.
//
// # Framing
//
// Every RESP value starts with one type byte and ends with CRLF:
//
//	+OK\r\n                       simple string   (a line; cannot contain CRLF)
//	-ERR unknown command\r\n      error           (a line; the first word is a code)
//	:1000\r\n                     integer
//	$5\r\nhello\r\n               bulk string     (length, then exactly that many bytes)
//	$-1\r\n                       null bulk string
//	*2\r\n$4\r\nINFO\r\n$6\r\nmemory\r\n   array (count, then that many values)
//
// RESP3 (Redis 6+, after HELLO 3) adds null (_), boolean (#), double (,),
// big number ((), verbatim string (=), blob error (!), map (%), set (~) and
// push (>). The client here never sends HELLO, so a server answers in RESP2,
// but the reader accepts those types too: they are a few lines each, and a
// proxy or a future server default should not turn into a protocol error.
//
// A command is always an array of bulk strings. That is the whole write side
// ([AppendCommand]).
//
// # Why length prefixes make a hostile server survivable
//
// A bulk string and an aggregate announce their size before their content.
// The reader can therefore refuse a reply *before* allocating for it: a
// "$2000000000\r\n" from a broken or malicious server is an error, not a 2 GB
// make([]byte). [Limits] bounds each dimension a reply can grow in — one bulk
// string, one aggregate's element count, nesting depth, and the total memory
// one reply may cost — so the worst case of reading from any server is a
// bounded allocation and an error, never a panic, a hang (deadlines come from
// the context) or an out-of-memory. Lines (simple strings, errors, headers)
// have no length prefix, so they are bounded by scanning: a line longer than
// the bulk limit without a CRLF is an error too.
//
// The element count is only a claim. "*65536\r\n" followed by nothing would
// make a naive reader allocate 65536 slots up front; this one grows the slice
// as elements actually arrive, and charges every element against the total
// budget, so memory tracks bytes received rather than bytes promised.
//
// # INFO is one bulk string
//
// INFO does not return structured data: it returns a single bulk string of
// "# Section" headers and "field:value" lines, meant for humans and for
// exactly this kind of scraper. That is why the protocol part of the check is
// small and [ParseInfo] does the real work: split lines, track the section,
// split each line at its first colon (values such as executable paths contain
// more), tolerate CRLF, blank lines and fields this code has never heard of —
// every Redis release adds some.
//
// # Counters and gauges in INFO
//
// INFO mixes two kinds of number, and the check must treat them differently
// ([Fields] records which is which):
//
//   - gauges are levels at this instant — used_memory, connected_clients,
//     mem_fragmentation_ratio — and are reported as they are;
//   - counters are running totals since the server started —
//     total_commands_processed, keyspace_hits, evicted_keys — and are only
//     meaningful as a rate: the delta between two scrapes over the time
//     between them. A restart resets them to zero, which a rate computation
//     must detect (the value went down) rather than report as a huge
//     negative rate.
//
// instantaneous_ops_per_sec looks like a rate but is Redis's own sample of
// the last few operations, a gauge; ops/s computed from
// total_commands_processed is the one that agrees with what the server did
// between two scrapes.
package resp
