package intake

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

type fakeLogs struct {
	mu   sync.Mutex
	logs []wire.Log
	err  error
}

func (f *fakeLogs) Append(_ context.Context, l []wire.Log) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.logs = append(f.logs, l...)
	return nil
}

type fakeHub struct{ got []wire.Log }

func (h *fakeHub) Publish(l []wire.Log) { h.got = append(h.got, l...) }

func (e *env) postLogs(body string, gz bool) (*httptest.ResponseRecorder, wire.IntakeResponse) {
	var rd *bytes.Reader
	if gz {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		_, _ = zw.Write([]byte(body))
		_ = zw.Close()
		rd = bytes.NewReader(b.Bytes())
	} else {
		rd = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/logs", rd)
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var out wire.IntakeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func logJSON(msg string) string {
	return fmt.Sprintf(`{"ts":%d,"message":%q,"status":"info","service":"web-api"}`, now.UnixMilli(), msg)
}

func TestLogs_StoredPublishedAndCounted(t *testing.T) {
	store, hub := &fakeLogs{}, &fakeHub{}
	e := newEnv(t, Options{Logs: store, LogHub: hub})
	body := `{"logs":[` + logJSON("a") + `,{"ts":5,"message":"too early","status":"info","service":"x"},` + logJSON("b") + `]}`
	rec, resp := e.postLogs(body, true)
	if rec.Code != http.StatusAccepted || resp.Accepted != 2 || resp.Rejected != 1 || len(resp.Errors) != 1 {
		t.Fatalf("%d %+v", rec.Code, resp)
	}
	if len(store.logs) != 2 || len(hub.got) != 2 || hub.got[0].Message != "a" {
		t.Fatalf("stored %d, published %d", len(store.logs), len(hub.got))
	}
	if e.reg.Counter("ozy.intake.logs_accepted").Value() != 2 || e.reg.Counter("ozy.intake.logs_rejected").Value() != 1 {
		t.Fatal("counters wrong")
	}
}

// If the store fails the agent resends the batch, so nothing may have been
// published yet or a tail would show every log twice.
func TestLogs_AStoreFailureIs503AndPublishesNothing(t *testing.T) {
	store, hub := &fakeLogs{err: errors.New("disk full")}, &fakeHub{}
	e := newEnv(t, Options{Logs: store, LogHub: hub})
	rec, _ := e.postLogs(`{"logs":[`+logJSON("a")+`]}`, false)
	if rec.Code != http.StatusServiceUnavailable || len(hub.got) != 0 {
		t.Fatalf("%d, %d published", rec.Code, len(hub.got))
	}
	if e.reg.Counter("ozy.intake.logs_accepted").Value() != 0 {
		t.Fatal("counted logs it did not store")
	}
}

func TestLogs_BadBodiesAndMissingStore(t *testing.T) {
	e := newEnv(t, Options{Logs: &fakeLogs{}})
	for body, want := range map[string]int{
		`not json`:              http.StatusBadRequest,
		`{}`:                    http.StatusBadRequest,
		`{"logs":[]}`:           http.StatusAccepted,
		`{"logs":[{"ts":"x"}]}`: http.StatusAccepted, // a bad log is a rejection, not a 400
	} {
		if rec, _ := e.postLogs(body, false); rec.Code != want {
			t.Errorf("%q: %d, want %d", body, rec.Code, want)
		}
	}
	none := newEnv(t, Options{})
	if rec, _ := none.postLogs(`{"logs":[]}`, false); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no store: %d", rec.Code)
	}
	var many []string
	for i := 0; i <= wire.MaxLogsPerRequest; i++ {
		many = append(many, logJSON("x"))
	}
	if rec, _ := e.postLogs(`{"logs":[`+strings.Join(many, ",")+`]}`, false); rec.Code != http.StatusBadRequest {
		t.Errorf("over the per-request limit: %d", rec.Code)
	}
}

func TestLogs_ErrorListIsBounded(t *testing.T) {
	e := newEnv(t, Options{Logs: &fakeLogs{}})
	var bad []string
	for i := 0; i < 50; i++ {
		bad = append(bad, `{"ts":1,"message":"m","status":"info","service":"x"}`)
	}
	_, resp := e.postLogs(`{"logs":[`+strings.Join(bad, ",")+`]}`, false)
	if resp.Rejected != 50 || len(resp.Errors) != wire.MaxResponseErrors {
		t.Fatalf("%d rejected, %d errors listed", resp.Rejected, len(resp.Errors))
	}
}
