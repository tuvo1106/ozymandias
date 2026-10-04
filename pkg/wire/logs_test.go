package wire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var logNow = time.UnixMilli(1_790_000_000_000)

func decodeLogs(t *testing.T, body string) ([]Log, []LogRejection, error) {
	t.Helper()
	return DecodeLogs([]byte(body), DecodeOptions{Now: logNow})
}

func TestDecodeLogs_AcceptsTheDocumentedExample(t *testing.T) {
	logs, rejects, err := decodeLogs(t, `{"logs":[{
		"ts": 1790000000123, "message": "api", "status": "info", "service": "web-api",
		"source": "winston", "host": "host-1", "tags": ["version:1.2.0", "env:dev"],
		"attrs": {"tag": "api", "method": "GET", "path": "/api/comics", "status_code": 200, "ms": 12},
		"trace_id": "0123456789abcdef0123456789abcdef", "span_id": "0123456789abcdef"}]}`)
	if err != nil || len(rejects) != 0 || len(logs) != 1 {
		t.Fatalf("logs=%d rejects=%v err=%v", len(logs), rejects, err)
	}
	l := logs[0]
	if l.Ts != 1790000000123 || l.Service != "web-api" || l.Status != "info" {
		t.Errorf("log = %+v", l)
	}
	if strings.Join(l.Tags, ",") != "env:dev,version:1.2.0" {
		t.Errorf("tags are not canonical (sorted): %v", l.Tags)
	}
}

func TestDecodeLogs_RejectsOneBadLogAndKeepsTheRest(t *testing.T) {
	cases := map[string]string{
		"no service":           `{"ts":1790000000000,"message":"m","status":"info"}`,
		"unknown status":       `{"ts":1790000000000,"message":"m","status":"fatal","service":"s"}`,
		"zero ts":              `{"ts":0,"message":"m","status":"info","service":"s"}`,
		"future ts":            `{"ts":1790001000000,"message":"m","status":"info","service":"s"}`,
		"seconds not millis":   `{"ts":1790000000,"message":"m","status":"info","service":"s"}`,
		"bad trace id":         `{"ts":1790000000000,"message":"m","status":"info","service":"s","trace_id":"xyz"}`,
		"uppercase trace id":   `{"ts":1790000000000,"message":"m","status":"info","service":"s","trace_id":"0123456789ABCDEF0123456789ABCDEF"}`,
		"trace id too long":    `{"ts":1790000000000,"message":"m","status":"info","service":"s","trace_id":"0123456789abcdef0123456789abcdef0"}`,
		"service with DEL":     `{"ts":1790000000000,"message":"m","status":"info","service":"a\u007fb"}`,
		"bad span id":          `{"ts":1790000000000,"message":"m","status":"info","service":"s","span_id":"12"}`,
		"bad tag":              `{"ts":1790000000000,"message":"m","status":"info","service":"s","tags":["no colon"]}`,
		"attrs not an object":  `{"ts":1790000000000,"message":"m","status":"info","service":"s","attrs":[1]}`,
		"service with newline": `{"ts":1790000000000,"message":"m","status":"info","service":"a\nb"}`,
		"service too long":     `{"ts":1790000000000,"message":"m","status":"info","service":"` + strings.Repeat("a", MaxLabelLen+1) + `"}`,
	}
	good := `{"ts":1790000000000,"message":"ok","status":"warn","service":"s"}`
	for name, bad := range cases {
		logs, rejects, err := decodeLogs(t, `{"logs":[`+good+`,`+bad+`,`+good+`]}`)
		if err != nil {
			t.Errorf("%s: whole body failed: %v", name, err)
			continue
		}
		if len(logs) != 2 || len(rejects) != 1 || rejects[0].Index != 1 {
			t.Errorf("%s: logs=%d rejects=%v, want the middle one refused", name, len(logs), rejects)
		}
	}
}

func TestDecodeLogs_AttrsTooLargeIsRejectedAndNumbersKeepEveryDigit(t *testing.T) {
	big := `{"ts":1790000000000,"message":"m","status":"info","service":"s","attrs":{"x":"` + strings.Repeat("a", MaxAttrsBytes) + `"}}`
	if logs, rejects, _ := decodeLogs(t, `{"logs":[`+big+`]}`); len(logs) != 0 || len(rejects) != 1 {
		t.Errorf("oversized attrs: logs=%d rejects=%v", len(logs), rejects)
	}
	// 2^53 + 1 is not a float64: the id must survive as written.
	logs, _, _ := decodeLogs(t, `{"logs":[{"ts":1790000000000,"message":"m","status":"info","service":"s","attrs":{"order_id":9007199254740993}}]}`)
	if len(logs) != 1 {
		t.Fatal("log refused")
	}
	out, _ := json.Marshal(logs[0])
	if !strings.Contains(string(out), `"order_id":9007199254740993`) {
		t.Errorf("a large integer lost digits: %s", out)
	}
}

// The flag truncation adds must not push attrs over their own limit: whatever
// DecodeLogs returns has to pass ValidateLog again.
func TestDecodeLogs_TruncationLeavesRoomForItsFlag(t *testing.T) {
	// Attrs sized to the exact limit once serialized: {"x":"aaa…"} is 8 bytes of frame.
	attrs := map[string]any{"x": strings.Repeat("a", MaxAttrsBytes-8)}
	body, _ := json.Marshal(map[string]any{"logs": []any{map[string]any{
		"ts": 1790000000000, "message": strings.Repeat("m", MaxMessageBytes+1), "status": "info", "service": "s", "attrs": attrs}}})
	logs, rejects, _ := DecodeLogs(body, DecodeOptions{Now: logNow})
	if len(logs) != 0 || len(rejects) != 1 {
		t.Fatalf("a long message with full attrs: logs=%d rejects=%v (the flag would not fit)", len(logs), rejects)
	}
	// The same attrs with a short message are fine, and re-validate.
	body, _ = json.Marshal(map[string]any{"logs": []any{map[string]any{
		"ts": 1790000000000, "message": "short", "status": "info", "service": "s", "attrs": attrs}}})
	logs, _, _ = DecodeLogs(body, DecodeOptions{Now: logNow})
	if len(logs) != 1 || ValidateLog(&logs[0], DecodeOptions{Now: logNow}) != nil {
		t.Fatalf("full attrs with a short message were refused")
	}
}

func TestDecodeLogs_LongMessageIsTruncatedAndFlagged(t *testing.T) {
	// A multibyte rune straddling the cut must not be split.
	msg := strings.Repeat("a", MaxMessageBytes-1) + "é" // é is 2 bytes: starts at the last allowed byte
	body, _ := json.Marshal(map[string]any{"logs": []any{map[string]any{
		"ts": 1790000000000, "message": msg, "status": "info", "service": "s"}}})
	logs, rejects, err := DecodeLogs(body, DecodeOptions{Now: logNow})
	if err != nil || len(rejects) != 0 || len(logs) != 1 {
		t.Fatalf("logs=%d rejects=%v err=%v", len(logs), rejects, err)
	}
	if len(logs[0].Message) > MaxMessageBytes || !validUTF8(logs[0].Message) {
		t.Errorf("message is %d bytes, valid utf8 = %v", len(logs[0].Message), validUTF8(logs[0].Message))
	}
	if logs[0].Attrs["_truncated"] != true {
		t.Errorf("attrs._truncated = %v, want true", logs[0].Attrs["_truncated"])
	}
	// An exactly-at-the-limit message is left alone and unflagged.
	exact := strings.Repeat("b", MaxMessageBytes)
	body, _ = json.Marshal(map[string]any{"logs": []any{map[string]any{
		"ts": 1790000000000, "message": exact, "status": "info", "service": "s"}}})
	logs, _, _ = DecodeLogs(body, DecodeOptions{Now: logNow})
	if len(logs) != 1 || logs[0].Message != exact || logs[0].Attrs["_truncated"] != nil {
		t.Errorf("a message at the limit was altered")
	}
}

func TestDecodeLogs_BodyErrorsAndLimits(t *testing.T) {
	for _, body := range []string{`not json`, `[]`, `{}`, `{"logs":null}`} {
		if _, _, err := decodeLogs(t, body); err == nil {
			t.Errorf("%q accepted", body)
		}
	}
	one := `{"ts":1790000000000,"message":"m","status":"info","service":"s"}`
	over := `{"logs":[` + strings.Repeat(one+",", MaxLogsPerRequest) + one + `]}`
	if _, _, err := decodeLogs(t, over); err == nil {
		t.Error("more than MaxLogsPerRequest accepted")
	}
	exact := `{"logs":[` + strings.Repeat(one+",", MaxLogsPerRequest-1) + one + `]}`
	if logs, _, err := decodeLogs(t, exact); err != nil || len(logs) != MaxLogsPerRequest {
		t.Errorf("exactly the limit: %d logs, err %v", len(logs), err)
	}
}

func TestDecodeLogs_MaxAge(t *testing.T) {
	old := `{"logs":[{"ts":1789000000000,"message":"m","status":"info","service":"s"}]}`
	if logs, _, _ := DecodeLogs([]byte(old), DecodeOptions{Now: logNow}); len(logs) != 1 {
		t.Error("an old log was refused with no MaxAge set")
	}
	logs, rejects, _ := DecodeLogs([]byte(old), DecodeOptions{Now: logNow, MaxAge: time.Hour})
	if len(logs) != 0 || len(rejects) != 1 {
		t.Errorf("a log older than MaxAge: logs=%d rejects=%v", len(logs), rejects)
	}
}

func TestStatusRank_OrdersTheFiveLevels(t *testing.T) {
	want := []string{"debug", "info", "warn", "error", "critical"}
	for i, s := range want {
		if !ValidLogStatus(s) {
			t.Errorf("%q invalid", s)
		}
		if i > 0 && LogStatusRank(s) <= LogStatusRank(want[i-1]) {
			t.Errorf("%q does not outrank %q", s, want[i-1])
		}
	}
	if ValidLogStatus("fatal") || LogStatusRank("fatal") != -1 {
		t.Error("an unknown level was accepted")
	}
}

// L4: DecodeLogs never panics, and whatever it accepts is valid and re-encodes.
func FuzzDecodeLogs(f *testing.F) {
	f.Add([]byte(`{"logs":[{"ts":1790000000000,"message":"m","status":"info","service":"s","attrs":{"a":[1,{"b":null}]}}]}`))
	f.Add([]byte(`{"logs":[{"ts":1790000000000,"message":"é","status":"error","service":"s","trace_id":"0123456789abcdef0123456789abcdef"}]}`))
	f.Add([]byte(`{"logs":[]}`))
	f.Add([]byte(`{"logs":[null,1,"x"]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		logs, _, err := DecodeLogs(body, DecodeOptions{Now: logNow})
		if err != nil {
			return
		}
		for i := range logs {
			if err := ValidateLog(&logs[i], DecodeOptions{Now: logNow}); err != nil {
				t.Fatalf("accepted log fails validation: %v", err)
			}
			if _, err := json.Marshal(logs[i]); err != nil {
				t.Fatalf("accepted log does not re-encode: %v", err)
			}
		}
	})
}
