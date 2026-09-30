// Package redis is the redis check: it connects to a Redis server once per
// run, asks INFO, and reports what the server says about its memory,
// clients, traffic and keyspace as redis.* (docs/metrics-catalog.md).
//
// # The protocol
//
// Redis speaks RESP: every value is one type byte ('+' simple string, '-'
// error, ':' integer, '$' bulk string, '*' array), its content, and CRLF. A
// command is an array of bulk strings, so INFO is five bytes of framing
// around four letters. The reply is one bulk string of "field:value" lines
// grouped under "# Section" headers — text written for people, which
// collector/resp parses into fields. The check writes nothing of its own on
// the wire; it only uses collector/resp's bounded reader and client.
//
// # Levels and totals
//
// INFO mixes two kinds of number. used_memory, connected_clients and
// uptime are levels: the value now is the answer, and they are reported as
// gauges. total_commands_processed, keyspace_hits and evicted_keys are
// running totals since the server started: 5 million commands says nothing
// about whether the server is busy, but 1500 more than 15 seconds ago says
// 100 per second. Those are turned into per-second rates between two runs
// (collector.Rates), which skips the first run and a server restart (the
// total goes down) rather than inventing a spike. Redis's own
// instantaneous_ops_per_sec is a sample over its last few hundred
// milliseconds, noisy and blind to anything between two scrapes; the rate of
// the total covers the whole interval.
//
// # Failure
//
// redis.can_connect is 1 or 0 on every run, so an unreachable server is a
// value a monitor can alert on rather than a gap. The run's error says why
// (refused, timeout, wrong password) and the scheduler logs it once. The
// password is sent in AUTH and appears nowhere else: not in the error, not in
// a tag.
package redis
