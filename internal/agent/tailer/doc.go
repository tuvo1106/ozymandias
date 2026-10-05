// Package tailer follows log files and container output reliably: it reads
// what is new, hands it to a [logpipeline.Pipeline] and then to a [Sink], and
// remembers how far it got only once the sink has accepted what it was given.
//
// # The mental model
//
// Delivery is at-least-once, and the whole guarantee is one ordering: a
// position (a file offset, a container timestamp) goes into the [Registry]
// only after the batch holding the lines before it was acknowledged. A crash,
// a restart or a failed send can therefore repeat lines but never skip them.
// Duplicates are the price, and deduplicating is a non-goal: the store keeps
// both copies.
//
//	file / container stream
//	      │  read at our own offset (no shared cursor, so rewinding is just a number)
//	      ▼
//	  lines ──► multiline join ──► pipeline ──► Sink.Send ──► ack ──► Registry
//
// # Files
//
// [Files] polls instead of watching: file-system events do not cross the bind
// mounts and VM boundaries the agent often reads through. A file is its
// (device, inode), not its path, because a path is soon another file after
// rotation. A renamed file keeps its inode, so reading continues where it
// was; the new file at the old path is a new inode and starts at zero. A
// file first seen when the agent starts begins at its end (do not replay
// history, configurable), but one that appears later is a rotation product and
// begins at zero. A file whose size drops below the offset was truncated in
// place (copytruncate) and restarts. A line over MaxLineBytes is truncated,
// and a last line with no newline is emitted after PartialFlushAfter.
//
// # Containers
//
// [Docker] reads /containers/{id}/logs. Without a TTY the stream is frames
// ([demuxer]), and a frame is not a line, so lines are reassembled per stream.
// Every line carries the daemon's timestamp, which is the resume point: on
// reconnect the agent asks for everything since the last acknowledged
// timestamp and drops lines at or before it. A container opts in, out, and
// configures itself with `ozy.logs.*` labels, so an app the agent has never
// heard of needs no agent config.
//
// Discovery polls the container list on the scan interval instead of sharing
// the M3 event watcher: the watcher answers "what happened", while log
// collection needs "what is running now", and a poll cannot miss a start
// event. The cost is up to one scan interval before a new container is
// followed, and its first lines are not lost because a container that starts
// after the agent is read from its first line.
//
// # Known limits
//
// A Rails request still open inside the pipeline when the agent dies is lost,
// because its lines were consumed and their offset committed. A crash between
// a send and a registry write repeats lines. A line longer than the cap that
// arrives without a newline commits an offset inside it, so a crash re-reads
// its tail as a line of its own.
package tailer
