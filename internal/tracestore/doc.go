// Package tracestore stores spans in Pebble with keys designed for the
// questions APM asks: fetch a whole trace by id, search entry spans by
// service, resource, duration and error, and derive the service map.
//
// Status: arrives in M5 (docs/plan/M5-tracing.md); this file marks its place in
// the architecture until then.
package tracestore
