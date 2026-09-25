// Package index is the inverted index from tag pairs to series ids (posting
// lists), and the set operations — intersection, union, negation — that turn a
// query's tag matchers into the series to read.
//
// # The mental model
//
// A query says `http.request.count{env:prod, service:web}`. Without an index
// the only way to answer it is to look at every series in the database and
// test its tags, which costs the size of the database no matter how small the
// answer is. The inverted index flips the direction: for each tag pair, store
// the sorted list of series that carry it. The query becomes an intersection
// of two sorted lists, costing roughly the size of the *smaller* one.
//
// Three consequences shape the code:
//
//   - Ids are assigned from a counter, so every list is sorted by
//     construction. Sorted lists are what make intersection linear and make
//     galloping possible.
//   - Positive matchers are intersected smallest-first — the result cannot be
//     bigger than the smallest input, so starting anywhere else is wasted
//     work.
//   - Negative matchers subtract at the end, never build a complement. The
//     complement of "not env:prod" is the whole database.
//
// # Cardinality, concretely
//
// The index is where a high-cardinality tag stops being an abstract worry:
// every distinct value adds a map entry and a list. A `user_id` tag on a
// million users is a million lists of one element each, and the memory is
// spent whether or not anyone queries by user. Size reports both numbers so
// the damage is visible, and the head enforces a per-metric series limit.
//
// # Relationship to the reference semantics
//
// [tsdb.Matcher.Matches] defines what a matcher means. This package is an
// optimization of that definition and must agree with it exactly — including
// the rule that a series *lacking* a key satisfies a not-equal matcher on it.
// A differential test checks the two against each other over random data;
// when they disagree, this package is wrong.
package index
