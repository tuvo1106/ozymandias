package testutil

import (
	"bytes"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// leakGrace is how long CheckGoroutines waits for goroutines to finish after
// the test body returns. Shutdown is asynchronous (a server's accept loop
// exits after Close returns), so an immediate check would report goroutines
// that are already on their way out.
const leakGrace = 2 * time.Second

// CheckGoroutines fails the test if goroutines started during it are still
// running shortly after it finishes. Call it first thing in the test:
//
//	func TestServer_Close(t *testing.T) {
//	    testutil.CheckGoroutines(t)
//	    ...
//	}
//
// It compares goroutine IDs before and after, so it must not be used in a
// test that calls t.Parallel (other tests' goroutines would count as leaks).
// HTTP clients keep idle connections alive in background goroutines: close
// them (client.CloseIdleConnections) before the test returns.
func CheckGoroutines(t T) {
	t.Helper()
	before := goroutineIDs(stacks())
	t.Cleanup(func() {
		t.Helper()
		deadline := time.Now().Add(leakGrace)
		var leaked []string
		for {
			leaked = leakedStacks(before, stacks())
			if len(leaked) == 0 {
				return
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(pollInterval)
		}
		t.Errorf("%d goroutine(s) leaked:\n\n%s", len(leaked), strings.Join(leaked, "\n\n"))
	})
}

// stacks returns the stack of every goroutine except the caller's.
func stacks() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	all := strings.Split(string(bytes.TrimSpace(buf)), "\n\n")
	return all[1:] // the first stack is the calling goroutine itself
}

// leakedStacks returns the stacks in now whose goroutine ID is not in before,
// ignoring goroutines owned by the test framework, sorted for stable output.
func leakedStacks(before map[int]bool, now []string) []string {
	var leaked []string
	for _, s := range now {
		id, ok := goroutineID(s)
		if !ok || before[id] || isFrameworkGoroutine(s) {
			continue
		}
		leaked = append(leaked, s)
	}
	sort.Strings(leaked)
	return leaked
}

func goroutineIDs(stacks []string) map[int]bool {
	ids := make(map[int]bool, len(stacks))
	for _, s := range stacks {
		if id, ok := goroutineID(s); ok {
			ids[id] = true
		}
	}
	return ids
}

// goroutineID parses the header line of a stack: "goroutine 12 [running]:".
func goroutineID(stack string) (int, bool) {
	rest, ok := strings.CutPrefix(stack, "goroutine ")
	if !ok {
		return 0, false
	}
	idStr, _, ok := strings.Cut(rest, " ")
	if !ok {
		return 0, false
	}
	id, err := strconv.Atoi(idStr)
	return id, err == nil
}

// isFrameworkGoroutine reports goroutines the testing package itself starts
// (other tests' runners, the timeout alarm) — never the code under test.
func isFrameworkGoroutine(stack string) bool {
	return strings.Contains(stack, "testing.tRunner") ||
		strings.Contains(stack, "testing.(*T).Run") ||
		strings.Contains(stack, "testing.runFuzzing") ||
		strings.Contains(stack, "created by testing.(*M)")
}
