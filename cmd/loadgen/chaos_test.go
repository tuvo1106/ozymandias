package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeOzyd serves the two read endpoints verify uses from what a test says is stored.
func fakeOzyd(t *testing.T, logs []int, points int) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/logs", func(w http.ResponseWriter, r *http.Request) {
		var ls []map[string]string
		for _, n := range logs {
			ls = append(ls, map[string]string{"message": fmt.Sprintf("chaos %d", n)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"logs": ls})
	})
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		pts := make([][]float64, points)
		for i := range pts {
			pts[i] = []float64{float64(i), 1}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "series": []any{map[string]any{"points": pts}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func writeAcks(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "acks")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// verify has to say no to each way the stores can disagree with the acks, or a drill
// built on it proves nothing: each case here is a way a real drill could go wrong.
func TestVerify_JudgesTheStoresAgainstTheAcks(t *testing.T) {
	acks := writeAcks(t,
		"s 0 a 1000", "l 0 a 1000", // acknowledged
		"s 1 a 1001", "l 1 a 1001",
		"s 2 u 1002", "l 2 u 1002", // unknown: may or may not have landed
	)
	for _, c := range []struct {
		name    string
		logs    []int
		points  int
		wantErr string
	}{
		{"all acked stored, unknown absent", []int{0, 1}, 2, ""},
		{"all acked stored, unknown present", []int{0, 1, 2}, 3, ""},
		{"an acked log is missing", []int{0}, 2, "acknowledged logs are missing"},
		{"a log stored twice", []int{0, 1, 1}, 2, "stored more than once"},
		{"a log nobody sent", []int{0, 1, 99}, 2, "never sent"},
		{"an acked point is missing", []int{0, 1}, 1, "acknowledged points are missing"},
		{"more points than acked plus unknown", []int{0, 1}, 4, "duplicates"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := verify(fakeOzyd(t, c.logs, c.points), acks, 0)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("unexpected failure: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("got %v, want an error containing %q", err, c.wantErr)
			}
		})
	}
}

func TestVerify_AnEmptyAcksFileIsNotAPass(t *testing.T) {
	p := filepath.Join(t.TempDir(), "acks")
	_ = os.WriteFile(p, nil, 0o644)
	if err := verify(fakeOzyd(t, nil, 0), p, 0); err == nil {
		t.Fatal("nothing was sent, and verify said all was well")
	}
}

func TestReadAcks_RefusesAMalformedLine(t *testing.T) {
	for _, bad := range []string{"l 0 a", "x", "l notanumber a 1", "l 0 a notatime"} {
		if _, err := readAcks(writeAcks(t, bad)); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// A 202 whose body says something was rejected is not a promise: the first version of
// the drill counted it as one and "verified" a store holding nothing.
func TestPost_OnlyACleanAcceptanceIsAnAck(t *testing.T) {
	for _, c := range []struct {
		code int
		body string
		want bool
	}{
		{202, `{"accepted":1,"rejected":0}`, true},
		{202, `{"accepted":0,"rejected":1}`, false},
		{202, `{"accepted":1,"rejected":1}`, false},
		{202, `not json`, false},
		{200, `{"accepted":1,"rejected":0}`, false},
		{503, `{"accepted":0,"rejected":0}`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.code)
			_, _ = w.Write([]byte(c.body))
		}))
		if got := post(&http.Client{Timeout: time.Second}, srv.URL, "{}"); got != c.want {
			t.Errorf("%d %s: acked=%v, want %v", c.code, c.body, got, c.want)
		}
		srv.Close()
	}
	if post(&http.Client{Timeout: time.Second}, "http://127.0.0.1:1", "{}") {
		t.Error("a refused connection was an ack")
	}
}

func TestAckIngest_ContinuesTheSequenceAndRecordsEachOutcome(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n%2 == 0 { // every second request is refused
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"accepted":1,"rejected":0}`))
	}))
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "acks")
	for range 2 { // two runs into one file, as a drill's kills would make
		if err := ackIngest(srv.URL, p, 200, 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	acks, err := readAcks(p)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var acked, unknown int
	for _, a := range acks {
		k := fmt.Sprintf("%c%d", a.kind, a.seq)
		if seen[k] {
			t.Fatalf("%s appears twice: a second run reused a sequence number", k)
		}
		seen[k] = true
		if a.acked {
			acked++
		} else {
			unknown++
		}
	}
	if acked == 0 || unknown == 0 {
		t.Fatalf("acked %d, unknown %d: both outcomes should have been recorded", acked, unknown)
	}
}

// Two runs into one acks file must not reuse a lane timestamp: the second would be a point
// the store drops as out of order, acknowledged all the same.
func TestAckIngest_ASecondRunNeverSendsALaneAnOldTimestamp(t *testing.T) {
	var stamps []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		if strings.Contains(string(b[:n]), chaosMetric) {
			stamps = append(stamps, string(b[:n]))
		}
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"accepted":1,"rejected":0}`))
	}))
	defer srv.Close()
	// A first run that left lane 1 far ahead of the clock. Its last sequence number is 64, so
	// this run starts at 65, which is lane 1 again: the lane that must not go backwards.
	future := time.Now().Unix() + 1000
	p := writeAcks(t, fmt.Sprintf("s 1 a %d", future), fmt.Sprintf("l 64 a %d", future))
	if err := ackIngest(srv.URL, p, 100, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, s := range stamps {
		if strings.Contains(s, `"lane:1"`) {
			seen++
		}
		if strings.Contains(s, `"lane:1"`) && !strings.Contains(s, fmt.Sprintf("[%d,1]", future+1)) && !strings.Contains(s, fmt.Sprintf("[%d,1]", future+2)) {
			t.Fatalf("lane 1 was sent a timestamp at or before the earlier run's %d: %s", future, s)
		}
	}
	if seen == 0 {
		t.Fatal("the run never sent lane 1, so the test checked nothing")
	}
}

// An empty page that still carries a cursor is not the end of the search.
func TestStoredLogs_KeepsPagingPastAnEmptyPageWithACursor(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = w.Write([]byte(`{"logs":[],"cursor":"c1"}`))
		case "c1":
			_, _ = w.Write([]byte(`{"logs":[{"message":"chaos 7"}],"cursor":""}`))
		}
	}))
	defer srv.Close()
	stored, _, err := storedLogs(srv.URL, 0, 1)
	if err != nil || stored[7] != 1 || calls != 2 {
		t.Fatalf("stored %v after %d calls, err %v: the empty first page ended the search", stored, calls, err)
	}
}
