package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits on span payloads (§B, §F).
const (
	// MaxSpansPerRequest bounds one agent → ozyd body (§F); the agent's batcher splits.
	MaxSpansPerRequest = 5000
	// MaxChunksPerRequest and MaxSpansPerChunk bound an SDK → agent body (§B).
	// A chunk is one trace's spans from one process; the tracer flushes a
	// partial chunk past 500 spans, so a chunk beyond 10x that is a bug or abuse.
	MaxChunksPerRequest = 1000
	MaxSpansPerChunk    = 5000
	// MaxSpanServiceLen bounds service; MaxSpanNameLen bounds name.
	MaxSpanServiceLen = 100
	MaxSpanNameLen    = 100
	// MaxResourceLen and MaxMetaValueLen are truncated, not refused: a long SQL
	// statement is evidence, and losing its tail beats losing the span.
	MaxResourceLen  = 5000
	MaxMetaValueLen = 5000
	// MaxMetaKeyLen bounds a meta or metrics key; MaxMetaEntries and
	// MaxMetricEntries bound how many a span carries (extra ones are dropped,
	// in key order, and counted by the receiver).
	MaxMetaKeyLen    = 100
	MaxMetaEntries   = 100
	MaxMetricEntries = 50
)

// Span types (§B). Anything else is normalized to SpanTypeCustom.
const (
	SpanTypeWeb    = "web"
	SpanTypeDB     = "db"
	SpanTypeCache  = "cache"
	SpanTypeQueue  = "queue"
	SpanTypeHTTP   = "http"
	SpanTypeWorker = "worker"
	SpanTypeCustom = "custom"
)

var spanTypes = map[string]bool{SpanTypeWeb: true, SpanTypeDB: true, SpanTypeCache: true, SpanTypeQueue: true, SpanTypeHTTP: true, SpanTypeWorker: true, SpanTypeCustom: true}

// Conventional metrics keys (§B).
const (
	MetricSamplingPriority = "_sampling_priority"
	MetricTopLevel         = "_top_level"
	MetricMeasured         = "_measured"
)

// Sampling priorities (§B): negative or zero drops, positive keeps.
const (
	PriorityUserDrop = -1
	PriorityAutoDrop = 0
	PriorityAutoKeep = 1
	PriorityUserKeep = 2
)

// Span is one unit of work on the wire (§B).
type Span struct {
	TraceID  string             `json:"trace_id"`
	SpanID   string             `json:"span_id"`
	ParentID string             `json:"parent_id,omitempty"` // "" for a trace root (null on the wire)
	Service  string             `json:"service"`
	Name     string             `json:"name"`
	Resource string             `json:"resource"`
	Type     string             `json:"type"`
	Start    int64              `json:"start"`    // unix microseconds
	Duration int64              `json:"duration"` // microseconds
	Error    int                `json:"error"`
	Meta     map[string]string  `json:"meta,omitempty"`
	Metrics  map[string]float64 `json:"metrics,omitempty"`
}

// End is when the span finished, in unix microseconds.
func (s *Span) End() int64 { return s.Start + s.Duration }

// TopLevel reports whether the span is a service entry span (§B `_top_level`).
func (s *Span) TopLevel() bool { return s.Metrics[MetricTopLevel] == 1 }

// Measured reports whether the span is forced into the RED statistics.
func (s *Span) Measured() bool { return s.Metrics[MetricMeasured] == 1 }

// TracerInfo identifies the sending library, for debugging a mixed fleet.
type TracerInfo struct {
	Lang        string `json:"lang"`
	LangVersion string `json:"lang_version"`
	Version     string `json:"version"`
}

// TracesPayload is a §B body: chunks of spans from one tracer.
type TracesPayload struct {
	Tracer TracerInfo `json:"tracer"`
	Traces [][]Span   `json:"traces"`
}

// SpansPayload is a §F body, agent → ozyd: a flat list after normalization and sampling.
type SpansPayload struct {
	Env   string `json:"env,omitempty"`
	Host  string `json:"host,omitempty"`
	Spans []Span `json:"spans"`
}

// SpanRejection says why one span was refused. Chunk is the chunk's index (-1
// for a flat §F body) and Index the span's within it.
type SpanRejection struct {
	Chunk, Index int
	Reason       string
}

func (r SpanRejection) String() string {
	if r.Chunk < 0 {
		return fmt.Sprintf("spans[%d]: %s", r.Index, r.Reason)
	}
	return fmt.Sprintf("traces[%d][%d]: %s", r.Chunk, r.Index, r.Reason)
}

// DecodeTraces parses a §B body. Each span is validated and normalized on its
// own: the valid ones come back as chunks (a chunk with every span refused is
// omitted), the invalid ones as rejections, and the error is for a body that is
// not a traces payload at all. Chunk boundaries are kept because they mean
// something: the spans of a chunk finished together in one process.
func DecodeTraces(body []byte, opts DecodeOptions) ([]TracerInfo, [][]Span, []SpanRejection, error) {
	var raw struct {
		Tracer TracerInfo          `json:"tracer"`
		Traces [][]json.RawMessage `json:"traces"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, nil, fmt.Errorf("body is not a traces payload: %w", err)
	}
	if raw.Traces == nil {
		return nil, nil, nil, errors.New(`body has no "traces" array`)
	}
	if len(raw.Traces) > MaxChunksPerRequest {
		return nil, nil, nil, fmt.Errorf("%d chunks in one request; the limit is %d", len(raw.Traces), MaxChunksPerRequest)
	}
	var chunks [][]Span
	var rejects []SpanRejection
	for ci, items := range raw.Traces {
		if len(items) > MaxSpansPerChunk {
			rejects = append(rejects, SpanRejection{Chunk: ci, Index: 0, Reason: fmt.Sprintf("chunk of %d spans; the limit is %d", len(items), MaxSpansPerChunk)})
			continue
		}
		var chunk []Span
		for i, item := range items {
			sp, err := decodeSpan(item, opts)
			if err != nil {
				rejects = append(rejects, SpanRejection{Chunk: ci, Index: i, Reason: err.Error()})
				continue
			}
			chunk = append(chunk, sp)
		}
		if len(chunk) > 0 {
			chunks = append(chunks, chunk)
		}
	}
	return []TracerInfo{raw.Tracer}, chunks, rejects, nil
}

// DecodeSpans parses a §F body the same way, flat.
func DecodeSpans(body []byte, opts DecodeOptions) (SpansPayload, []SpanRejection, error) {
	var raw struct {
		Env   string            `json:"env"`
		Host  string            `json:"host"`
		Spans []json.RawMessage `json:"spans"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return SpansPayload{}, nil, fmt.Errorf("body is not a spans payload: %w", err)
	}
	if raw.Spans == nil {
		return SpansPayload{}, nil, errors.New(`body has no "spans" array`)
	}
	if len(raw.Spans) > MaxSpansPerRequest {
		return SpansPayload{}, nil, fmt.Errorf("%d spans in one request; the limit is %d", len(raw.Spans), MaxSpansPerRequest)
	}
	out := SpansPayload{Env: raw.Env, Host: raw.Host, Spans: make([]Span, 0, len(raw.Spans))}
	var rejects []SpanRejection
	for i, item := range raw.Spans {
		sp, err := decodeSpan(item, opts)
		if err != nil {
			rejects = append(rejects, SpanRejection{Chunk: -1, Index: i, Reason: err.Error()})
			continue
		}
		out.Spans = append(out.Spans, sp)
	}
	return out, rejects, nil
}

func decodeSpan(item json.RawMessage, opts DecodeOptions) (Span, error) {
	var sp Span
	dec := json.NewDecoder(bytes.NewReader(item))
	// parent_id may be null: a *string reads null and absent alike as nil.
	var shadow struct {
		Span
		ParentID *string `json:"parent_id"`
	}
	if err := dec.Decode(&shadow); err != nil {
		return sp, err
	}
	sp = shadow.Span
	if shadow.ParentID != nil {
		sp.ParentID = *shadow.ParentID
	}
	if err := ValidateSpan(&sp, opts); err != nil {
		return sp, err
	}
	NormalizeSpan(&sp)
	return sp, nil
}

func allZero(s string) bool { return strings.Trim(s, "0") == "" }

// ValidSpanIDs reports whether the ids satisfy §B: 32 and 16 lowercase hex, neither
// all zero, and a parent that is empty or 16 hex and not the span itself.
func ValidSpanIDs(traceID, spanID, parentID string) error {
	if !isLowerHex(traceID, 32) || allZero(traceID) {
		return fmt.Errorf("trace_id %q is not 32 lowercase hex characters, or is zero", clip(traceID, 40))
	}
	if !isLowerHex(spanID, 16) || allZero(spanID) {
		return fmt.Errorf("span_id %q is not 16 lowercase hex characters, or is zero", clip(spanID, 24))
	}
	if parentID != "" {
		if !isLowerHex(parentID, 16) || allZero(parentID) {
			return fmt.Errorf("parent_id %q is not 16 lowercase hex characters, or is zero", clip(parentID, 24))
		}
		if parentID == spanID {
			return errors.New("parent_id equals span_id")
		}
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ValidateSpan checks one span against §B. It does not modify sp: a long
// resource or an unknown type passes, because those are normalizations.
func ValidateSpan(sp *Span, opts DecodeOptions) error {
	if err := ValidSpanIDs(sp.TraceID, sp.SpanID, sp.ParentID); err != nil {
		return err
	}
	if sp.Service == "" || len(sp.Service) > MaxSpanServiceLen || !utf8.ValidString(sp.Service) {
		return fmt.Errorf("service must be 1 to %d bytes of UTF-8", MaxSpanServiceLen)
	}
	if sp.Name == "" || len(sp.Name) > MaxSpanNameLen || !utf8.ValidString(sp.Name) {
		return fmt.Errorf("name must be 1 to %d bytes of UTF-8", MaxSpanNameLen)
	}
	if sp.Error != 0 && sp.Error != 1 {
		return fmt.Errorf("error must be 0 or 1, got %d", sp.Error)
	}
	if sp.Duration < 0 {
		return fmt.Errorf("duration %d is negative", sp.Duration)
	}
	if sp.Start <= 0 {
		return fmt.Errorf("start %d is not a positive unix time in microseconds", sp.Start)
	}
	if sp.Start < 1_000_000_000_000_000 {
		return fmt.Errorf("start %d looks like seconds or milliseconds; want unix microseconds", sp.Start)
	}
	if !opts.Now.IsZero() {
		nowUs := opts.Now.UnixMicro()
		if latest := nowUs + MaxFutureSkew.Microseconds(); sp.Start > latest {
			return fmt.Errorf("start %d is more than %s in the future", sp.Start, MaxFutureSkew)
		}
		if opts.MaxAge > 0 && sp.End() < nowUs-opts.MaxAge.Microseconds() {
			return fmt.Errorf("span ended more than %s ago", opts.MaxAge)
		}
	}
	for k, v := range sp.Metrics {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("metrics[%q] is not a finite number", clip(k, 40))
		}
	}
	return nil
}

// NormalizeSpan applies the §B rules that change a span instead of refusing it:
// an unknown type becomes custom, resource and meta values are cut to their
// limits on a rune boundary, and over-long keys or surplus entries are dropped
// (in sorted key order, so the same span always normalizes the same way).
func NormalizeSpan(sp *Span) {
	if !spanTypes[sp.Type] {
		sp.Type = SpanTypeCustom
	}
	sp.Resource = truncRunes(sp.Resource, MaxResourceLen)
	if len(sp.Meta) > 0 {
		sp.Meta = limitMap(sp.Meta, MaxMetaEntries, func(v string) string { return truncRunes(v, MaxMetaValueLen) })
	}
	if len(sp.Metrics) > 0 {
		sp.Metrics = limitMap(sp.Metrics, MaxMetricEntries, func(v float64) float64 { return v })
	}
}

func limitMap[V any](m map[string]V, maxEntries int, fix func(V) V) map[string]V {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "" && len(k) <= MaxMetaKeyLen {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	if len(keys) > maxEntries {
		keys = keys[:maxEntries]
	}
	out := make(map[string]V, len(keys))
	for _, k := range keys {
		out[k] = fix(m[k])
	}
	return out
}

// truncRunes cuts s to at most n bytes without splitting a rune.
func truncRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// SamplingMultiplier is Knuth's multiplicative hash constant, the one dd-trace
// uses for head sampling, so that a trace's decision depends on its id alone.
const SamplingMultiplier uint64 = 1111111111111111111

// SampleKeep is the head-sampling decision for a trace at a rate in [0, 1]: keep
// when (low 64 bits of the trace id * SamplingMultiplier) mod 2^64 is below
// rate * 2^64. It is a pure function of (trace_id, rate), so every service in a
// trace, in any language, agrees without talking to each other; the Python, Node
// and Go suites check the same vectors (testdata/traces/sampling.json). rate <= 0
// never keeps and rate >= 1 always does, whatever the id.
func SampleKeep(traceID string, rate float64) bool {
	if !(rate > 0) {
		return false
	}
	if rate >= 1 {
		return true
	}
	if len(traceID) != 32 {
		return false
	}
	var low uint64
	for i := 16; i < 32; i++ {
		c := traceID[i]
		var d uint64
		switch {
		case c >= '0' && c <= '9':
			d = uint64(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint64(c-'a') + 10
		default:
			return false
		}
		low = low<<4 | d
	}
	// rate * 2^64 is exact in a float64 (scaling by a power of two), and below 2^64
	// for rate < 1, so the conversion is exact too.
	threshold := uint64(rate * (1 << 64))
	return low*SamplingMultiplier < threshold
}

// NormalizePath turns a URL path into a low-cardinality route resource (§B,
// "Path normalizer"): segments that are all digits, UUIDs, hex of 12 or more,
// or nanoid-like (16 or more of [A-Za-z0-9_-] containing a digit) become ":id",
// and the result keeps at most 8 segments. Every SDK implements this and checks
// testdata/traces/normalize-path.json, so a route has one spelling everywhere.
func NormalizePath(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	if path == "" || path == "/" {
		return "/"
	}
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segs) > 8 {
		segs = segs[:8]
	}
	for i, s := range segs {
		if isIDSegment(s) {
			segs[i] = ":id"
		}
	}
	return "/" + strings.Join(segs, "/")
}

func isIDSegment(s string) bool {
	if s == "" {
		return false
	}
	digits, hex, uuid := true, true, isUUID(s)
	hasDigit, nano := false, true
	for i := 0; i < len(s); i++ {
		c := s[i]
		isDigit := c >= '0' && c <= '9'
		if isDigit {
			hasDigit = true
		} else {
			digits = false
		}
		if !isDigit && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			hex = false
		}
		if !isDigit && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '_' && c != '-' {
			nano = false
		}
	}
	return digits || uuid || (hex && len(s) >= 12) || (nano && len(s) >= 16 && hasDigit)
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// ParsePropagation reads the three propagation header values (§B). ok is false
// for anything malformed: garbage never yields a context, and never panics.
func ParsePropagation(traceID, parentID, priority string) (tid, pid string, prio int, ok bool) {
	traceID, parentID = strings.ToLower(strings.TrimSpace(traceID)), strings.ToLower(strings.TrimSpace(parentID))
	if err := ValidSpanIDs(traceID, parentID, ""); err != nil {
		return "", "", 0, false
	}
	prio = PriorityAutoKeep
	switch strings.TrimSpace(priority) {
	case "":
	case "-1":
		prio = PriorityUserDrop
	case "0":
		prio = PriorityAutoDrop
	case "1":
		prio = PriorityAutoKeep
	case "2":
		prio = PriorityUserKeep
	default:
		return "", "", 0, false
	}
	return traceID, parentID, prio, true
}

// SpanDuration converts a span's duration for display.
func SpanDuration(sp *Span) time.Duration { return time.Duration(sp.Duration) * time.Microsecond }
