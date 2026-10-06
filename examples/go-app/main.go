// Command go-app posts a three-span trace to a running agent with no SDK at
// all: only the wire protocol (docs/wire-protocol.md section B). It exists to
// show that the SDKs are conveniences, not a requirement; anything that can make
// an HTTP POST can be traced.
//
//	go run ./examples/go-app            # agent on localhost:8126
//	OZY_AGENT=http://agent:8126 go run ./examples/go-app
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

type span map[string]any

func id(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	b[0] |= 1 // never all zero
	return hex.EncodeToString(b)
}

func main() {
	agent := os.Getenv("OZY_AGENT")
	if agent == "" {
		agent = "http://localhost:8126"
	}
	trace, root, db := id(16), id(8), id(8)
	// The root span is "the request"; a database child is inside it. Times are unix
	// microseconds, and the duration is measured, not computed from two wall times.
	start := time.Now()
	time.Sleep(12 * time.Millisecond)
	dbDur := time.Since(start)
	time.Sleep(3 * time.Millisecond)
	total := time.Since(start)
	t0 := start.UnixMicro()

	body, _ := json.Marshal(map[string]any{
		"tracer": map[string]string{"lang": "go", "version": "example"},
		"traces": [][]span{{
			{"trace_id": trace, "span_id": root, "parent_id": nil, "service": "go-example", "name": "http.request", "resource": "GET /hello",
				"type": "web", "start": t0, "duration": total.Microseconds(), "error": 0,
				"meta": map[string]string{"env": "dev", "http.status_code": "200"}, "metrics": map[string]float64{"_top_level": 1, "_sampling_priority": 1}},
			{"trace_id": trace, "span_id": db, "parent_id": root, "service": "go-example", "name": "db.query", "resource": "select 1",
				"type": "db", "start": t0, "duration": dbDur.Microseconds(), "error": 0},
		}},
	})
	resp, err := http.Post(agent+"/v1/traces", "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, "is the agent running?", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	fmt.Printf("trace %s -> %s %s\n", trace, resp.Status, out)
}
