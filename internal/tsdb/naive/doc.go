// Package naive is the deliberately simple SQLite MetricStore: one row per
// sample, selection by scanning a metric's series and applying the
// reference matcher semantics in Go. It is the M1 tracer bullet's storage,
// and afterwards the permanent oracle the real TSDB is differentially tested
// against — so obviously-correct beats fast everywhere in it.
//
// It is also the baseline that makes M2 worth doing: its size on disk per
// sample and its query time over a day of data are measured in the M1 notes,
// and the TSDB's numbers are read against them.
package naive
