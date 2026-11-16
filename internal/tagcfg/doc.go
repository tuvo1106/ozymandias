// Package tagcfg holds the per-metric tag allow/deny rules and applies them.
//
// A metric whose series count is exploding almost always has one tag that carries an
// id, a timestamp or a URL. A rule says which tag keys that metric may have: "deny
// user_id" drops that key from every point of the metric, "allow env, service" keeps
// only those. The series the points then belong to are the ones without the tag.
//
// Where it is applied: at intake, before the queue (ADR-0062), not at the consumer as
// the plan first said. A rule applied at the consumer would change what a record means
// after it was acknowledged: a record queued before a rule is saved and consumed after
// would be stored under the old tags by one consumer run and the new tags by the
// next, and a redelivered record would be re-filtered by whatever the rules are by then.
// Applied at intake, every record on the queue already carries the tags it will be
// stored with, and redelivery is exactly idempotent.
//
// What it costs. Dropping a key merges series: two points that differed only in the
// dropped tag now belong to one series, and at the same timestamp the store keeps the
// first and refuses the rest as duplicates (counted under ozy.tsdb.ooo_rejected). That is
// the price of stopping a bomb, and a rule is meant for the tag that is one, not for a
// tag that splits real series. It is not a sum: a count split over two tags is not
// added back together.
package tagcfg
