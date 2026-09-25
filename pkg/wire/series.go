package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

// HTTP limits and headers shared by every agent → ozyd hop (§0).
const (
	// MaxBodyBytes bounds a request body as sent (gzip'd).
	MaxBodyBytes = 4 << 20
	// MaxDecompressedBytes bounds a body after gunzip — the zip-bomb guard.
	MaxDecompressedBytes = 16 << 20
	// MaxSeriesPerRequest bounds one /v1/series body; the forwarder splits.
	MaxSeriesPerRequest = 5000
	// MaxFutureSkew is how far ahead of the receiver's clock a point may be.
	// Beyond that it is a clock bug, and accepting it would pin a series'
	// "latest" value in the future.
	MaxFutureSkew = 10 * time.Minute

	HeaderAgentVersion = "X-Ozy-Agent-Version"
	HeaderHost         = "X-Ozy-Host"
	HeaderKey          = "X-Ozy-Key"
)

// Kind is how a series' values are to be read (§C). It is metadata about
// the metric, not the point: a count's value is "events in this interval",
// a gauge's is "the level at this instant", and they aggregate differently
// over time (sum vs. average) — which is why the query layer needs it.
type Kind string

// Kinds of series.
const (
	KindCount Kind = "count"
	KindRate  Kind = "rate"
	KindGauge Kind = "gauge"
)

// Point is one (timestamp, value) pair, [unix_seconds, float64] on the wire.
type Point struct {
	Timestamp int64
	Value     float64
}

// MarshalJSON writes the point as a two-element array. NaN and ±Inf have no
// JSON form and are refused rather than written as invalid JSON.
func (p Point) MarshalJSON() ([]byte, error) {
	if math.IsNaN(p.Value) || math.IsInf(p.Value, 0) {
		return nil, fmt.Errorf("point value %v is not finite", p.Value)
	}
	b := make([]byte, 0, 32)
	b = append(b, '[')
	b = strconv.AppendInt(b, p.Timestamp, 10)
	b = append(b, ',')
	b = strconv.AppendFloat(b, p.Value, 'g', -1, 64)
	return append(b, ']'), nil
}

// UnmarshalJSON reads [ts, value]. The timestamp must be an integer; a value
// too large for float64 is an error here rather than a silent +Inf.
func (p *Point) UnmarshalJSON(data []byte) error {
	var pair []json.Number
	if err := json.Unmarshal(data, &pair); err != nil {
		return fmt.Errorf("point: want [unix_seconds, value]: %w", err)
	}
	if len(pair) != 2 {
		return fmt.Errorf("point: want [unix_seconds, value], got %d elements", len(pair))
	}
	ts, err := pair[0].Int64()
	if err != nil {
		return fmt.Errorf("point timestamp %q: want integer unix seconds", pair[0])
	}
	v, err := strconv.ParseFloat(pair[1].String(), 64)
	if err != nil {
		return fmt.Errorf("point value %q: %w", pair[1], errors.Unwrap(err))
	}
	p.Timestamp, p.Value = ts, v
	return nil
}

// Series is one metric context and its points, as the agent flushes it (§C).
type Series struct {
	Metric string `json:"metric"`
	Type   Kind   `json:"type"`
	// Interval is the bucket width in seconds for count and rate; 0 for gauge.
	Interval int64    `json:"interval"`
	Tags     []string `json:"tags"`
	Points   []Point  `json:"points"`
}

// SeriesPayload is the body of POST /v1/series.
type SeriesPayload struct {
	Series []Series `json:"series"`
}

// IntakeResponse is the 202 body every intake endpoint returns (§0).
// Partial acceptance is success: one bad series must not make the agent
// retry the good ones.
type IntakeResponse struct {
	Status   string   `json:"status"`
	Accepted int      `json:"accepted"`
	Rejected int      `json:"rejected"`
	Errors   []string `json:"errors"`
}

// MaxResponseErrors bounds IntakeResponse.Errors; the counts carry the rest.
const MaxResponseErrors = 10

// ErrorResponse is the body of every 4xx/5xx from ozyd's HTTP APIs.
type ErrorResponse struct {
	Status string `json:"status"` // always "error"
	Error  string `json:"error"`
}

// DecodeOptions carries the receiver's view of time for validation.
type DecodeOptions struct {
	// Now is the receiver's clock; points beyond Now+MaxFutureSkew are rejected.
	Now time.Time
	// MaxAge rejects points older than Now-MaxAge. Zero accepts any age (the
	// M1 naive store has no retention window; M2's TSDB sets one).
	MaxAge time.Duration
}

// Rejection says why one series in a payload was refused.
type Rejection struct {
	Index  int    // position in the payload's series array
	Metric string // as sent, possibly invalid
	Reason string
}

func (r Rejection) String() string {
	return fmt.Sprintf("series[%d] %q: %s", r.Index, r.Metric, r.Reason)
}

// DecodeSeries parses a /v1/series body and validates each series on its own
// (§C): the valid ones come back with canonical (sorted, de-duplicated) tags,
// the invalid ones as rejections. The error is reserved for a body that is
// not a series payload at all — the only case where a 400 is the right answer.
//
// The unit of rejection is the series, not the point: a series with one bad
// point is refused whole. Agents send one or a few points per series, and a
// rule this simple is easy for any third-party sender to predict.
func DecodeSeries(body []byte, opts DecodeOptions) ([]Series, []Rejection, error) {
	var raw struct {
		Series []json.RawMessage `json:"series"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, fmt.Errorf("body is not a series payload: %w", err)
	}
	if raw.Series == nil {
		return nil, nil, errors.New(`body has no "series" array`)
	}
	if len(raw.Series) > MaxSeriesPerRequest {
		return nil, nil, fmt.Errorf("%d series in one request; the limit is %d", len(raw.Series), MaxSeriesPerRequest)
	}
	valid := make([]Series, 0, len(raw.Series))
	var rejects []Rejection
	for i, item := range raw.Series {
		var s Series
		if err := json.Unmarshal(item, &s); err != nil {
			rejects = append(rejects, Rejection{Index: i, Metric: s.Metric, Reason: err.Error()})
			continue
		}
		if err := ValidateSeries(&s, opts); err != nil {
			rejects = append(rejects, Rejection{Index: i, Metric: s.Metric, Reason: err.Error()})
			continue
		}
		s.Tags = CanonicalTags(s.Tags)
		valid = append(valid, s)
	}
	return valid, rejects, nil
}

// ValidateSeries checks one series against §C. It does not modify s.
func ValidateSeries(s *Series, opts DecodeOptions) error {
	if !ValidMetricName(s.Metric) {
		return fmt.Errorf("invalid metric name (want ^[a-zA-Z][a-zA-Z0-9_.]{0,%d}$)", MaxMetricNameLen-1)
	}
	switch s.Type {
	case KindCount, KindRate:
		if s.Interval <= 0 {
			return fmt.Errorf("type %s needs a positive interval, got %d", s.Type, s.Interval)
		}
	case KindGauge:
		if s.Interval != 0 {
			return fmt.Errorf("type gauge must have interval 0, got %d", s.Interval)
		}
	default:
		return fmt.Errorf("unknown type %q (want count, rate or gauge)", s.Type)
	}
	if len(s.Tags) > MaxTagsPerPoint {
		return fmt.Errorf("%d tags; the limit is %d", len(s.Tags), MaxTagsPerPoint)
	}
	for _, t := range s.Tags {
		if !ValidTag(t) {
			return fmt.Errorf("invalid tag %q", t)
		}
	}
	if len(s.Points) == 0 {
		return errors.New("no points")
	}
	latest := opts.Now.Add(MaxFutureSkew).Unix()
	for _, p := range s.Points {
		if math.IsNaN(p.Value) || math.IsInf(p.Value, 0) {
			return fmt.Errorf("point at %d is not a finite number", p.Timestamp)
		}
		if p.Timestamp <= 0 {
			return fmt.Errorf("point timestamp %d is not a positive unix time", p.Timestamp)
		}
		if p.Timestamp > latest {
			return fmt.Errorf("point at %d is more than %s in the future", p.Timestamp, MaxFutureSkew)
		}
		if opts.MaxAge > 0 && p.Timestamp < opts.Now.Add(-opts.MaxAge).Unix() {
			return fmt.Errorf("point at %d is older than the accepted window (%s)", p.Timestamp, opts.MaxAge)
		}
	}
	return nil
}
