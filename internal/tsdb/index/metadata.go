package index

import "sort"

// The metadata queries behind the UI's autocomplete: what metrics exist, what
// tag keys a metric carries, what values a key takes. They are written over
// [Lookup] rather than over one index, for the same reason [Select] is — the
// answers must not change when a series ages from the head into a block.
//
// All three are exhaustive scans of the key or value space. That is the honest
// cost of an inverted index: it is built to answer "which series have this
// tag", and "what tags are there" runs against the grain. Real systems keep a
// separate metadata store for exactly this (ozymandias does too, from M3), and
// these functions are what fills it.

// Metrics returns the indexed metric names with the given prefix, sorted.
func Metrics(l Lookup, prefix string) []string {
	var out []string
	for _, name := range l.TagValues(MetricName) {
		if prefix == "" || hasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Keys returns the tag keys in l, sorted, excluding the metric-name pseudo
// key. If metric is non-empty, only keys carried by that metric's series.
func Keys(l Lookup, metric string) []string {
	var ids []uint64
	if metric != "" {
		if ids = l.Postings(MetricName, metric); len(ids) == 0 {
			return nil
		}
	}
	var out []string
	for _, key := range l.TagKeys() {
		if key == MetricName {
			continue
		}
		if ids == nil {
			out = append(out, key)
			continue
		}
		for _, v := range l.TagValues(key) {
			if intersects(l.Postings(key, v), ids) {
				out = append(out, key)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// Values returns the values seen for a tag key, sorted, optionally restricted
// to series of one metric.
func Values(l Lookup, metric, key string) []string {
	var ids []uint64
	if metric != "" {
		if ids = l.Postings(MetricName, metric); len(ids) == 0 {
			return nil
		}
	}
	var out []string
	for _, v := range l.TagValues(key) {
		if ids == nil || intersects(l.Postings(key, v), ids) {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
