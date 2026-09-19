package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

// syncBuffer is a bytes.Buffer safe to read while a running command writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runner abstracts the two binaries so each scenario runs against both.
type runner func(ctx context.Context, args, env []string, stdout, stderr io.Writer) int

var binaries = map[string]runner{
	"ozyd": func(ctx context.Context, args, env []string, o, e io.Writer) int {
		return Ozyd(ctx, args, env, o, e, nil)
	},
	"agent": Agent,
}

// envFor gives each binary a config that listens on an ephemeral loopback
// port, logs JSON (so the test can find the "listening" line) and keeps state
// in a temp dir.
func envFor(t *testing.T, name string) []string {
	if name == "ozyd" {
		return []string{
			"OZY_HTTP_ADDR=127.0.0.1:0",
			"OZY_DATA_DIR=" + t.TempDir(),
			"OZY_LOG_FORMAT=json",
		}
	}
	return []string{
		"OZY_AGENT_HTTP_ADDR=127.0.0.1:0",
		"OZY_AGENT_CONFD_PATH=" + t.TempDir(),
		"OZY_AGENT_HOSTNAME=test-host",
		"OZY_AGENT_LOG_FORMAT=json",
	}
}

func TestVersionFlag(t *testing.T) {
	for name, run := range binaries {
		var out bytes.Buffer
		if code := run(context.Background(), []string{"-version"}, nil, &out, io.Discard); code != ExitOK {
			t.Errorf("%s -version exit = %d", name, code)
		}
		if got := out.String(); got != name+" dev\n" {
			t.Errorf("%s -version = %q", name, got)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	for name, run := range binaries {
		for _, args := range [][]string{{"-nope"}, {"extra"}, {"healthcheck", "extra"}} {
			var errOut bytes.Buffer
			if code := run(context.Background(), args, nil, io.Discard, &errOut); code != ExitUsage {
				t.Errorf("%s %v: exit = %d, want %d", name, args, code, ExitUsage)
			}
			if !strings.Contains(errOut.String(), "usage:") && !strings.Contains(errOut.String(), "flag provided") {
				t.Errorf("%s %v: stderr = %q, want usage", name, args, errOut.String())
			}
		}
		if code := run(context.Background(), []string{"-h"}, nil, io.Discard, io.Discard); code != ExitOK {
			t.Errorf("%s -h: exit = %d, want 0", name, code)
		}
	}
}

func TestConfigErrorsExitWithUsageCode(t *testing.T) {
	for name, run := range binaries {
		var errOut bytes.Buffer
		code := run(context.Background(), []string{"-config", "/does/not/exist.yaml"}, nil, io.Discard, &errOut)
		if code != ExitUsage || !strings.Contains(errOut.String(), "exist.yaml") {
			t.Errorf("%s: exit = %d stderr = %q", name, code, errOut.String())
		}
	}
}

// The full lifecycle of each binary, in-process: start on an ephemeral port,
// answer /healthz, log unknown env vars as warnings, stop cleanly on cancel.
func TestRun_StartsServesAndStopsOnCancel(t *testing.T) {
	for name, run := range binaries {
		t.Run(name, func(t *testing.T) {
			testutil.CheckGoroutines(t)
			var stderr syncBuffer
			env := append(envFor(t, name), prefixOf(name)+"TYPO=1")
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan int, 1)
			go func() { done <- run(ctx, nil, env, io.Discard, &stderr) }()

			var addr string
			testutil.Eventually(t, 5*time.Second, func() bool {
				addr = listeningAddr(stderr.String())
				return addr != ""
			}, "no listening line in:\n%s", &stderr)
			if err := httpserve.Probe(context.Background(), "http://"+addr+"/healthz", 2*time.Second); err != nil {
				t.Fatalf("probe: %v", err)
			}
			if !strings.Contains(stderr.String(), prefixOf(name)+"TYPO") {
				t.Errorf("unknown env var not warned about:\n%s", stderr.String())
			}
			cancel()
			if code := <-done; code != ExitOK {
				t.Fatalf("exit = %d, stderr:\n%s", code, stderr.String())
			}
		})
	}
}

func TestRun_RuntimeFailureExitsWithFailureCode(t *testing.T) {
	taken, err := httpserve.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	for name, run := range binaries {
		env := append(envFor(t, name), prefixOf(name)+"HTTP_ADDR="+taken.Addr().String())
		var stderr bytes.Buffer
		if code := run(context.Background(), nil, env, io.Discard, &stderr); code != ExitFailure {
			t.Errorf("%s on a taken port: exit = %d, stderr = %s", name, code, stderr.String())
		}
	}
}

func TestRun_StartupErrorExitsWithFailureCode(t *testing.T) {
	// ozyd's startup check: an unusable data dir.
	env := []string{"OZY_DATA_DIR=/dev/null/impossible", "OZY_HTTP_ADDR=127.0.0.1:0"}
	if code := Ozyd(context.Background(), nil, env, io.Discard, io.Discard, nil); code != ExitFailure {
		t.Errorf("unusable data dir: exit = %d, want %d", code, ExitFailure)
	}
}

func TestHealthcheck(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer healthy.Close()
	healthyAddr := strings.TrimPrefix(healthy.URL, "http://")

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	for name, run := range binaries {
		base := envFor(t, name)
		var out bytes.Buffer
		env := append(base, prefixOf(name)+"HTTP_ADDR="+healthyAddr)
		if code := run(context.Background(), []string{"healthcheck"}, env, &out, io.Discard); code != ExitOK ||
			!strings.Contains(out.String(), "ok") {
			t.Errorf("%s healthy: exit = %d out = %q", name, code, out.String())
		}
		var errOut bytes.Buffer
		env = append(base, prefixOf(name)+"HTTP_ADDR="+closedAddr)
		if code := run(context.Background(), []string{"healthcheck"}, env, io.Discard, &errOut); code != ExitFailure ||
			!strings.Contains(errOut.String(), "unhealthy") {
			t.Errorf("%s unhealthy: exit = %d err = %q", name, code, errOut.String())
		}
	}
}

func TestNewLogger_TextFormat(t *testing.T) {
	var buf bytes.Buffer
	cfgEnv := []string{"OZY_LOG_FORMAT=text", "OZY_LOG_LEVEL=warn",
		"OZY_HTTP_ADDR=127.0.0.1:0", "OZY_DATA_DIR=" + t.TempDir(), "OZY_TYPO=1"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	Ozyd(ctx, nil, cfgEnv, io.Discard, &buf, nil)
	// At warn level only the unknown-variable warning is printed, as text.
	if out := buf.String(); !strings.Contains(out, "level=WARN") || strings.Contains(out, "listening") {
		t.Fatalf("log output = %q", out)
	}
}

func prefixOf(name string) string {
	if name == "ozyd" {
		return "OZY_"
	}
	return "OZY_AGENT_"
}

// listeningAddr finds the address in the JSON "listening" log line.
func listeningAddr(logs string) string {
	for _, line := range strings.Split(logs, "\n") {
		var rec struct {
			Msg  string `json:"msg"`
			Addr string `json:"addr"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "listening" {
			return rec.Addr
		}
	}
	return ""
}
