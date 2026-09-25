// Package logql parses the log search syntax (service:api status:error
// "timeout" @duration:>500) and splits each query into stream matchers
// answered by the index and line filters answered by scanning.
//
// Status: arrives in M4 (docs/plan/M4-logs.md); this file marks its place in
// the architecture until then.
package logql
