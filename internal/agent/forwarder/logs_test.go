package forwarder

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

func sinkFor(t *testing.T, h http.HandlerFunc) (*LogSink, *selfmetrics.Registry) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	reg := selfmetrics.NewRegistry()
	return NewLogSink(LogSinkOptions{URL: srv.URL, Hostname: "box", Version: "v9", Registry: reg, Logger: slog.New(slog.DiscardHandler)}), reg
}

var oneLog = []wire.Log{{Ts: 1790000000000, Message: "m", Status: "info", Service: "svc"}}

func TestLogSink_PostsGzippedJSONWithIdentityHeaders(t *testing.T) {
	var got wire.LogsPayload
	var hdr http.Header
	s, reg := sinkFor(t, func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		b, _ := io.ReadAll(zr)
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"ok","accepted":1,"rejected":0,"errors":[]}`))
	})
	if err := s.Send(context.Background(), oneLog); err != nil {
		t.Fatal(err)
	}
	if len(got.Logs) != 1 || got.Logs[0].Message != "m" || hdr.Get("Content-Encoding") != "gzip" ||
		hdr.Get(wire.HeaderHost) != "box" || hdr.Get(wire.HeaderAgentVersion) != "v9" {
		t.Fatalf("%+v %v", got, hdr)
	}
	if reg.Counter("ozy.agent.logs.sent").Value() != 1 {
		t.Fatal("sent not counted")
	}
}

func TestLogSink_WhatIsAnErrorAndWhatIsDropped(t *testing.T) {
	for _, tc := range []struct {
		status  int
		wantErr bool
	}{
		{202, false}, {200, false},
		{400, false}, {413, false}, // poison batches are dropped, not retried forever
		{429, true}, {500, true}, {503, true}, {404, true}, {401, true},
	} {
		s, _ := sinkFor(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"status":"error","error":"x"}`))
		})
		err := s.Send(context.Background(), oneLog)
		if (err != nil) != tc.wantErr {
			t.Errorf("status %d: err=%v, wantErr=%v", tc.status, err, tc.wantErr)
		}
	}
}

func TestLogSink_CountsRejectionsAndRefusals(t *testing.T) {
	var n atomic.Int32
	s, reg := sinkFor(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"status":"ok","accepted":0,"rejected":3,"errors":["logs[0]: x"]}`))
			return
		}
		w.WriteHeader(400)
	})
	_ = s.Send(context.Background(), oneLog)
	_ = s.Send(context.Background(), oneLog)
	if reg.Counter("ozy.agent.logs.rejected").Value() != 3 || reg.Counter("ozy.agent.logs.batches_refused").Value() != 1 {
		t.Fatalf("rejected %d refused %d", reg.Counter("ozy.agent.logs.rejected").Value(), reg.Counter("ozy.agent.logs.batches_refused").Value())
	}
}

func TestLogSink_NetworkErrorsAndEmptyBatches(t *testing.T) {
	s, reg := sinkFor(t, func(http.ResponseWriter, *http.Request) {})
	s.url = "http://127.0.0.1:1/v1/logs" // nothing listens
	if err := s.Send(context.Background(), oneLog); err == nil || !strings.Contains(err.Error(), "posting logs") {
		t.Fatalf("%v", err)
	}
	if reg.Counter("ozy.agent.logs.send_errors").Value() != 1 {
		t.Fatal("send error not counted")
	}
	if err := s.Send(context.Background(), nil); err != nil {
		t.Fatal("an empty batch has nothing to send")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Send(ctx, oneLog); err == nil {
		t.Fatal("a cancelled context must fail the send")
	}
}
