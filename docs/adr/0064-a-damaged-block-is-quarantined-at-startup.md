# ADR-0064: A damaged TSDB block is quarantined at startup, not fatal

- **Status:** Accepted
- **Date:** 2026-11-20

## Context

Until now ozyd refused to start if any block of the metric store failed to open. That is loud and it is safe,
and it makes one flipped bit in one two-hour block a full outage: in queued mode, no intake, until someone logs
in and moves a directory by hand. M7's plan asks for the opposite behaviour ("detected, block quarantined, rest
served, loud self-metric + log"), and ADR-0061 recorded that doing so reverses the refusal on purpose and left it
as the owner's call. The owner decided on 2026-11-20: quarantine, loudly, and detect what opening already reads,
not every byte of every chunk.

## Decision

**At startup a block whose own files are damaged is moved aside and ozyd starts without it.** The rest of the
store is served.

- **What is damage, and nothing else is.** The sentinel is `block.ErrCorrupt`. It marks: a `meta.json` that does
  not parse or ends before it starts; an `index.dat` that is missing, too short, has the wrong magic, fails its
  whole-file checksum, or whose table of contents or sections do not decode; a `chunks.dat` that is missing, too
  short for its header, has the wrong magic, or **is shorter than the index says it must be** (the index lists
  every chunk's offset and length, so a truncated chunks file is seen at open without reading a chunk); and a block
  directory that has files in it and no `meta.json`.
- **What is not damage stays fatal.** A block written by a different version of the software (`meta.json`, index
  or chunks version) is whole, and a build that cannot read it must not hide it. An I/O error (`EIO`, `EMFILE`,
  `EACCES`, a failed stat) says nothing about the block, which may be perfectly healthy. Both still stop the start,
  with the block where it was: moving a good block aside because of a transient fault is the one mistake a
  quarantine cannot afford.
- **The move.** The directory is renamed to `<ulid>.corrupt-<unix seconds>` in the same `blocks/` directory and the
  parent is synced. **Nothing is deleted or edited**: the files are exactly as found. If the move fails, that is an
  error and ozyd does not start; carrying on as if it had succeeded would serve a hole and say nothing.
- **A quarantined directory is nobody's to touch.** `OpenAll` skips it, `CleanTmp` skips it (it would otherwise
  remove an empty or meta-less one), compaction and retention work from the list of open blocks and never see it.
  It is the operator's to inspect, restore (put the files right, take `.corrupt-<unix>` off the name, restart) or
  remove.
- **A block that lost its `meta.json` is quarantined, not deleted.** `CleanTmp` used to remove any directory without
  one on the reasoning that it was an interrupted write. It is not: a write is a `.tmp` directory until the rename
  that publishes it, and deletion writes a tombstone first, removes `meta.json` second, and `CleanCondemned` finishes
  tombstoned directories before `CleanTmp` runs. By the time `CleanTmp` looks, nothing but damage explains a final-named
  directory with files and no `meta.json`. An empty one is still removed (there is nothing in it to lose).
- **Loud, in three places.** Each quarantined block is one `ERROR` log line with the block, where it went, the cause
  and the time range it covered (`min_time`, `max_time`, when `meta.json` could still be read). The gauge
  `ozy.tsdb.blocks_quarantined` counts the quarantined directories **on disk**, so it survives a restart and falls only
  when an operator restores or removes one. `ozy.tsdb.corrupt_reads` counts reads that found damage the open cannot
  see (next point).
- **`/readyz` stays 200.** A quarantined block is data missing from old time ranges, not a store that stopped: new data
  is accepted and stored correctly. Failing readiness would take the instance out of rotation and stop intake to
  protect reads of data that is already gone, and an operator who wants ozyd to stop on corruption can alert on the
  gauge. The signal is the metric and the log.
- **What opening does not read.** A flipped bit inside a chunk's body is found by the query that reads that chunk,
  whose checksum fails: the query returns an error wrapping `ErrCorrupt` and **no samples**, never a wrong value. The
  failure is counted (`corrupt_reads`) and logged once per block, not once per request. **It is not quarantined while ozyd
  runs, and a restart does not find it either**, because opening still does not read the chunk. Until an operator moves
  that block aside by hand (the log names its directory), every query over its time range fails. See Consequences.

Rejected alternatives:

- **Keep refusing to start.** Loud and safe, and one flipped bit is a full outage that needs a person.
- **Read every chunk at startup.** It would catch a flipped bit in a chunk body, and cost a read of all the data at
  every start, growing with it. The owner chose not to.
- **Delete the damaged block.** Irreversible, and a block with one bad chunk still holds good data an operator may want.
  A mistaken detection must be undoable by a rename.
- **A config switch between quarantine and refuse.** More surface for a decision that is reversible by hand. An operator
  who prefers a hard stop can alert on `ozy.tsdb.blocks_quarantined` and take the instance out.
- **Quarantine on any open error.** Hides a good block on `EMFILE` or after a downgrade.

## Consequences

- **Answers can have a hole, and the API does not say so.** While `ozy.tsdb.blocks_quarantined` is not zero, queries over
  the time a quarantined block covered return less than was stored, with no per-query marker; the API has no notion of a
  partial result. This is the real trade against refusing to start, and it is accepted. The mitigation is the gauge and the log
  line, so **a dashboard or alert must watch the gauge**; the runbook says so.
- **A bad chunk body keeps failing queries until someone acts.** The read-time detection is loud but does not remove the
  block. A runtime quarantine (take the block out of the live list while queries hold it, and while compaction might be
  reading it) is a larger change with races to get right, and is not done. Compaction reading such a block fails the same
  way each time it plans the run; that predates this ADR.
- **Quarantined directories take disk that nothing reclaims.** They do not count toward `storage.max_bytes` and never
  expire. Backups hard-link the whole `blocks/` directory, so a restored backup brings them back, still quarantined.
- **Not covered.** The Pebble-backed stores (traces, sketches) have their own corruption handling and this does not touch
  it. **A real bit-rot was not observed**: the tests damage files the way a test can (flipped bytes, truncation, removal),
  and the durability of the rename itself has no crash-simulating filesystem here (the parent is synced; a power cut
  between the rename and the sync is not tested).
- **A downgrade is still a refusal.** A newer block under an older build stops the start, as it always did.
