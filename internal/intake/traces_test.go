package intake

import (
	"bytes"
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

type fakeSpans struct {
	mu    sync.Mutex
	spans []wire.Span
	err   error
}

func (f *fakeSpans) Append(_ context.Context, s []wire.Span) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.spans = append(f.spans, s...)
	return nil
}

func spanBody(n int, extra string) string {
	return fmt.Sprintf(`{"trace_id":"%032x","span_id":"%016x","service":"api","name":"http.request","resource":"GET /","type":"web","start":%d,"duration":10,"error":0%s}`,
		n, n, now.UnixMicro(), extra)
}

func (e *env) postSpans(body string) (*httptest.ResponseRecorder, wire.IntakeResponse) {
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var out wire.IntakeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestTraces_StoredCountedAndDefaultsFilled(t *testing.T) {
	st := &fakeSpans{}
	e := newEnv(t, Options{Traces: st})
	rec, resp := e.postSpans(`{"env":"dev","host":"h1","spans":[` + spanBody(1, "") + `,` + spanBody(2, `,"meta":{"env":"prod"}`) + `,{"trace_id":"bad"}]}`)
	if rec.Code != http.StatusAccepted || resp.Accepted != 2 || resp.Rejected != 1 || len(resp.Errors) != 1 {
		t.Fatalf("%d %+v", rec.Code, resp)
	}
	if st.spans[0].Meta["env"] != "dev" || st.spans[0].Meta["host"] != "h1" || st.spans[1].Meta["env"] != "prod" {
		t.Errorf("env/host defaults: %v %v", st.spans[0].Meta, st.spans[1].Meta)
	}
}

func TestTraces_StoreFailureIs503AndNothingCounted(t *testing.T) {
	e := newEnv(t, Options{Traces: &fakeSpans{err: errors.New("disk full")}})
	rec, _ := e.postSpans(`{"spans":[` + spanBody(1, "") + `]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("%d", rec.Code)
	}
}

func TestTraces_NoStoreAndBadBodies(t *testing.T) {
	if rec, _ := newEnv(t, Options{}).postSpans(`{"spans":[]}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no store: %d", rec.Code)
	}
	e := newEnv(t, Options{Traces: &fakeSpans{}})
	for name, body := range map[string]string{"not json": `{`, "no spans": `{"env":"x"}`, "too many": `{"spans":[` + strings.Repeat(`{},`, wire.MaxSpansPerRequest) + `{}]}`} {
		if rec, _ := e.postSpans(body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}
