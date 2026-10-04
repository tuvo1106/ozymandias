# ADR-0040: Per-block trigram bloom filters, in chunk format v2

- **Status:** Accepted
- **Date:** 2026-10-04

## Context

`docs/plan/M4-logs.md` §3 sketches the second half of the log store: at seal time, tokenize each
block's message and string attribute values into a bloom filter (lowercase, split on
non-alphanumerics, keep tokens of 3 to 40 characters), store it beside the block, and let free-text
and `@attr:value` terms skip blocks that cannot contain them.

That sketch has a hole the first version of the code found. A free-text term in logql is a
**case-insensitive substring** (`time` finds "timeout"; `tim*out` has a wildcard), and a filter of
whole words cannot answer a substring query. Using it anyway means either a skip that is sometimes
wrong (a log that exists and is never returned, with no error) or changing what a search means to
"whole word", which is a regression for every query already written. A bloom filter is only worth
having if skipping on it can never lose a log.

## Decision

**The filter holds every three-byte window of the lowercased text, not words.** A substring of three
or more bytes occurs in a text only if all its windows do, so absent windows prove absence, for the
semantics as they are. The text is the message and every string, number and boolean attribute value
of every log in the block, lowercased with `strings.ToLower`, which is what the matcher does to the
text it searches; attribute names and stream labels are not in it.

**A query is turned into what a block must contain, and that derivation is sound by construction.**
`logql.Needs` returns a disjunction of conjunctions of literals: a literal is a run of at least three
bytes between wildcards of a free-text term or a non-numeric `@attr:value`. Everything else (labels,
`NOT`, numeric comparisons, a numeric-looking `@attr:1000` because numbers compare as numbers and
`1e3` would match it, a literal shorter than three bytes) constrains nothing, an `OR` with an
unconstrained arm is unconstrained, and a product that would grow past 16 alternatives is replaced by
a weaker one. A block is skipped only if every alternative has an absent window. A Need may be weaker
than possible and never stronger, and that is tested against the real matcher over random text and
queries (`TestBloom_NeverRulesOutABlockHoldingAMatch`) and end to end against brute force
(`TestProperty_SearchEqualsBruteForce`, with generators that make blocks differ).

**The filter is stored beside the block, in a new chunk format version.** Version 2 adds `bloomLen`
to the block header and the index entry and the filter after the compressed data; version 1 chunks
read as before and a v1 file keeps its layout when a newer binary appends to it (one file never mixes
layouts; the next day's chunk is the first v2). The filter ends in its own crc32c: a flipped bit that
turns 1 into 0 would deny a log that is there, so a filter that fails its checksum, or has an unknown
hash count, is ignored and the block is read in full. See `docs/formats/log-chunk.md`.

**Sizing: 8 bits per distinct window, `k = 4`, capped at 64 KiB.** About a 2.4% false-positive rate
per window, and a term of n bytes needs n-2 windows to be wrong at once. A skipped block costs no
scan budget.

## Alternatives considered

| Option | Why not |
|---|---|
| Whole-word (or 3 to 40 character token) filter, as the plan says | Cannot answer a substring search. Either unsound or a change to what a search means |
| Change free-text to whole-word semantics so a word filter is sound | Breaks every existing query (`time` for "timeout") to make an optimization easier; the user's search semantics are not the storage layer's to change |
| A sidecar `.bloom` file per chunk, leaving the chunk format alone | No format bump, but a second file whose agreement with the chunk is a thing to get wrong (crash between the two, a block rewritten, a retention delete of one). A filter inside the block it describes cannot disagree with it; v1 compat is a test, not a hope |
| Larger n-grams (4 or 5) | Fewer distinct windows and a smaller filter, but a literal of three or four bytes (`500`, `err`) could not use it. Three bytes is the shortest window that is worth probing |
| A bigger filter (more bits per window) | Cheap to change later (it is a constant and the format records `k` and the length), and the measured skip rate on a rare word is already above 99%; revisit if a workload's words are common |
| Keep an in-memory cache of filters across queries | A filter read is a few KiB from the page cache; the decompression it avoids is hundreds of times larger. Not worth the invalidation code until a profile says otherwise |

## Consequences

- A rare word, an id or an error code reads a small fraction of the blocks it used to. Measured on a
  60,000-log corpus in 120 blocks (docs/notes/M4.md): a word in one place read 1 block of 120 and
  went from 78 ms to under 1 ms; a word in no block skipped all 120.
- A word in most blocks (a common word, `ab`, a number comparison, a label) skips nothing and pays one
  small read per block that the unfiltered path did not. The cost is not measurable against the
  decompression it sits in front of, and the bytes the filter adds are 22% of the chunk files on that
  corpus (the test asserts under 30%).
- The filter's content is now part of the format: changing the tokenizer or hash changes what every
  stored v2 chunk answers, so it needs a version bump and new goldens. The golden pins the filter
  bytes.
- Terms the filter cannot speak to are still fully scanned, as before. A query with a label or a
  numeric comparison and no free text gets nothing from it, which is correct: the index already
  narrowed it.
- Aggregates and facets skip blocks the same way, because they share the snapshot.
