// Package backup is the file-level half of ozyd's backup and restore: how files
// get into a backup directory, the manifest that vouches for them, and the
// restore that checks the manifest before it writes anything.
//
// # What a backup is
//
// A backup is a directory with one subdirectory per store, laid out as
// data_dir is, and a MANIFEST.json listing every file with its size and
// SHA-256. The stores write their own parts (each knows what is immutable and
// what is being appended to, and what its own lock is); this package makes the
// whole thing checkable. Restoring a backup is copying the tree into an empty
// data_dir and starting ozyd, which recovers it as it recovers from a crash:
// that is the property each store's backup has to give.
//
// # Consistency without stopping the world
//
// Each store is snapshotted consistently under its own locks while ozyd keeps
// serving. The stores are snapshotted at slightly different moments, which is
// safe because of the order, not because anything pauses:
//
//   - The queue is cut first. Its snapshot holds the committed offsets as they
//     were at the cut, and every record at or above them.
//   - The stores are cut after. A store snapshot may therefore hold records the
//     queue's snapshot also still holds (they were applied after the queue's
//     cut): on restore they are delivered again, and every store applies a
//     record idempotently.
//
// The other order would lose data: a store cut first and a queue cut second can
// record an offset as committed whose records the store's snapshot never saw.
// The one gap is the log store's: its head dedupe does not see a log that was
// sealed into a chunk between the two cuts, so such a log can be stored twice
// after a restore (docs/notes/M7.md, "log redelivery after a seal").
//
// Hard links are used where a file is immutable (blocks, SSTables) and the
// backup is on the same filesystem; a copy where it is not. A hard-linked file
// is the same bytes as the live one, so a backup made by links protects against
// ozyd's own bugs and operator mistakes but not against the loss of the disk:
// put the backup directory on another volume for that.
package backup
