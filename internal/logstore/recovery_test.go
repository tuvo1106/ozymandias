package logstore

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// crashCopy copies the data directory as it is on disk right now, which is
// what a process killed at this instant would leave: files closed or not,
// whatever the store had written and not yet written. Opening the copy is
// recovery from that crash.
func crashCopy(t testing.TB, src string) string {
	t.Helper()
	dst, err := os.MkdirTemp("", "logstore-crash")
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// everything returns the message of every log in the store, sorted, so a test
// can say "each of these exactly once".
func everything(t testing.TB, s *Store) []string {
	t.Helper()
	var out []string
	for _, l := range all(t, s, "", wideFrom, wideTo, SearchOpts{Limit: 1000}) {
		out = append(out, l.Message)
	}
	sort.Strings(out)
	return out
}

func wantMessages(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%03d", prefix, i)
	}
	sort.Strings(out)
	return out
}

func batchOf(prefix string, from, n int, ts int64) []Log {
	out := make([]Log, n)
	for i := range out {
		out[i] = mkLog(ts+int64(from+i), "api", "info", fmt.Sprintf("%s%03d", prefix, from+i), 1)
	}
	return out
}

// Log aliases the wire type so the helpers above read short.
type Log = wire.Log

func equal(a, b []string) bool { return strings.Join(a, "\n") == strings.Join(b, "\n") }

func openSynced(t testing.TB, dir string) *Store {
	t.Helper()
	s, _ := openStore(t, dir, func(o *Options) { o.NoSync = false })
	return s
}

func TestRecovery_UnsealedLogsComeBackFromTheWAL(t *testing.T) {
	dir := t.TempDir()
	s := openSynced(t, dir)
	defer func() { _ = s.Close() }()
	base := t0.UnixMilli()
	if err := s.Append(context.Background(), batchOf("m", 0, 30, base)); err != nil {
		t.Fatal(err)
	}
	// No flush, no close: the logs are only in the WAL and the head.
	crashed := openSynced(t, crashCopy(t, dir))
	defer func() { _ = crashed.Close() }()
	if got := everything(t, crashed); !equal(got, wantMessages("m", 30)) {
		t.Fatalf("after a crash with everything unsealed: %d logs, want 30 exactly once", len(got))
	}
	// And the recovered store carries on: new appends work and are not mixed up with old ones.
	if err := crashed.Append(context.Background(), batchOf("n", 0, 5, base+1000)); err != nil {
		t.Fatal(err)
	}
	if got := everything(t, crashed); len(got) != 35 {
		t.Fatalf("after appending to a recovered store: %d logs, want 35", len(got))
	}
}

// The window the ordering exists for: the block is durable and published, the
// WAL still holds the same entries. A crash here must not duplicate them.
func TestRecovery_CrashBetweenBlockAndWALTruncateDoesNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	s := openSynced(t, dir)
	defer func() { _ = s.Close() }()
	var snapshots []string
	s.afterBlock = func() { snapshots = append(snapshots, crashCopy(t, dir)) }
	base := t0.UnixMilli()
	total := 0
	for i := 0; i < 4; i++ { // enough to seal several blocks via the size threshold
		if err := s.Append(context.Background(), batchOf("m", total, 25, base)); err != nil {
			t.Fatal(err)
		}
		total += 25
	}
	if len(snapshots) == 0 {
		t.Fatal("no block was sealed; the test did not reach the window")
	}
	for i, snap := range snapshots {
		r := openSynced(t, snap)
		got := everything(t, r)
		_ = r.Close()
		// The snapshot was taken mid-run, so it holds a prefix of what was appended; the property
		// is that it holds each of its logs exactly once.
		seen := map[string]int{}
		for _, m := range got {
			seen[m]++
		}
		for m, n := range seen {
			if n != 1 {
				t.Fatalf("snapshot %d: %q appears %d times", i, m, n)
			}
		}
		if len(got) == 0 {
			t.Fatalf("snapshot %d recovered nothing", i)
		}
	}
	last := openSynced(t, snapshots[len(snapshots)-1])
	defer func() { _ = last.Close() }()
	// The snapshot is taken inside an Append, after everything before it was acknowledged.
	if got := len(everything(t, last)); got < 25*3 {
		t.Fatalf("the last snapshot recovered %d logs; everything acknowledged before the final Append is 75", got)
	}
}

// A block half-written when the process died: its entries must come back from
// the WAL, which is still whole because the WAL is only truncated after the
// block is durable.
func TestRecovery_TornChunkTailIsReplayedFromTheWAL(t *testing.T) {
	dir := t.TempDir()
	s := openSynced(t, dir)
	defer func() { _ = s.Close() }()
	var snap string
	s.afterBlock = func() {
		if snap == "" {
			snap = crashCopy(t, dir)
		}
	}
	base := t0.UnixMilli()
	for i := 0; i < 4; i++ {
		if err := s.Append(context.Background(), batchOf("m", i*25, 25, base)); err != nil {
			t.Fatal(err)
		}
	}
	if snap == "" {
		t.Fatal("no block sealed")
	}
	chunks := findFiles(t, snap, ".chunk")
	if len(chunks) == 0 {
		t.Fatal("snapshot has no chunk")
	}
	for _, c := range chunks {
		st, _ := os.Stat(c)
		if err := os.Truncate(c, st.Size()-7); err != nil { // tear the last block
			t.Fatal(err)
		}
	}
	r := openSynced(t, snap)
	defer func() { _ = r.Close() }()
	got := everything(t, r)
	seen := map[string]int{}
	for _, m := range got {
		seen[m]++
	}
	for m, n := range seen {
		if n != 1 {
			t.Fatalf("%q appears %d times", m, n)
		}
	}
	// The first block's entries were in the WAL (not yet truncated) when the snapshot was taken.
	if len(got) < 25 {
		t.Fatalf("recovered %d logs; the torn block's entries should have come back from the WAL", len(got))
	}
}

func TestRecovery_TornWALTailKeepsEverythingBeforeIt(t *testing.T) {
	dir := t.TempDir()
	s, _ := openStore(t, dir, func(o *Options) { o.NoSync = false; o.BlockBytes = 1 << 30 }) // nothing seals: both batches are only in the WAL
	defer func() { _ = s.Close() }()
	base := t0.UnixMilli()
	_ = s.Append(context.Background(), batchOf("a", 0, 10, base))
	_ = s.Append(context.Background(), batchOf("b", 0, 10, base+100))
	snap := crashCopy(t, dir)
	wals := findFiles(t, filepath.Join(snap, "wal"), ".wal")
	last := wals[len(wals)-1]
	st, _ := os.Stat(last)
	if err := os.Truncate(last, st.Size()-5); err != nil { // the second batch's record is cut short
		t.Fatal(err)
	}
	r := openSynced(t, snap)
	defer func() { _ = r.Close() }()
	got := everything(t, r)
	if !equal(got, wantMessages("a", 10)) {
		t.Fatalf("recovered %v; want exactly the first batch (the torn second batch was never complete)", got)
	}
	// The cut tail was removed, so new writes are not stranded behind it.
	if err := r.Append(context.Background(), batchOf("c", 0, 3, base+200)); err != nil {
		t.Fatal(err)
	}
	r2 := openSynced(t, crashCopy(t, snap))
	defer func() { _ = r2.Close() }()
	if got := everything(t, r2); len(got) != 13 {
		t.Fatalf("after appending to a repaired WAL and crashing again: %d logs, want 13", len(got))
	}
}

func findFiles(t testing.TB, root, suffix string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, suffix) {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func TestRecovery_ADamagedChunkDoesNotStopTheStore(t *testing.T) {
	dir := t.TempDir()
	s, _ := openStore(t, dir)
	base := t0.UnixMilli()
	_ = s.Append(context.Background(), batchOf("a", 0, 5, base))
	_ = s.Flush()
	// A second stream, on its own chunk file.
	other := mkLog(base, "web", "info", "web-line", 1)
	_ = s.Append(context.Background(), []Log{other})
	_ = s.Close()
	chunks := findFiles(t, dir, ".chunk")
	if len(chunks) != 2 {
		t.Fatalf("%d chunks", len(chunks))
	}
	if err := os.WriteFile(chunks[0], []byte("this is not a chunk"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The WAL still holds the sealed entries (it is truncated by segment); drop it,
	// so this tests what the chunks alone say.
	if err := os.RemoveAll(filepath.Join(dir, "wal")); err != nil {
		t.Fatal(err)
	}
	r, _ := openStore(t, dir)
	defer func() { _ = r.Close() }()
	if got := everything(t, r); len(got) != 1 {
		t.Fatalf("with one chunk destroyed the store returned %v, want the other stream's one log", got)
	}
}

func TestStore_FlushTruncatesTheWAL(t *testing.T) {
	dir := t.TempDir()
	s, _ := openStore(t, dir, func(o *Options) { o.WALSegmentSize = 4 << 10; o.BlockBytes = 1 << 30 })
	defer func() { _ = s.Close() }()
	base := t0.UnixMilli()
	for i := 0; i < 20; i++ {
		_ = s.Append(context.Background(), batchOf("m", i*20, 20, base))
	}
	before := len(findFiles(t, filepath.Join(dir, "wal"), ".wal"))
	if before < 4 {
		t.Fatalf("test setup: only %d WAL segments", before)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if after := len(findFiles(t, filepath.Join(dir, "wal"), ".wal")); after != 1 {
		t.Fatalf("%d WAL segments before the flush, %d after; sealed entries must not pin the WAL", before, after)
	}
	// And an unsealed stream pins its segments: the oldest unsealed entry's segment survives.
	_ = s.Append(context.Background(), batchOf("x", 0, 5, base))
	for i := 0; i < 20; i++ {
		_ = s.Append(context.Background(), []Log{mkLog(base, "web", "info", fmt.Sprint("w", i), 1)}) // other stream, sealed below
	}
	segs := len(findFiles(t, filepath.Join(dir, "wal"), ".wal"))
	var web *stream
	s.smu.RLock()
	for _, st := range s.byID {
		if st.labels.value("service") == "web" {
			web = st
		}
	}
	s.smu.RUnlock()
	s.wmu.Lock()
	_ = s.seal(web)
	s.wmu.Unlock()
	if after := len(findFiles(t, filepath.Join(dir, "wal"), ".wal")); after > segs {
		t.Fatalf("sealing grew the WAL: %d -> %d", segs, after)
	}
	crashed := openSynced(t, crashCopy(t, dir))
	defer func() { _ = crashed.Close() }()
	if got := len(everything(t, crashed)); got != 400+5+20 {
		t.Fatalf("recovered %d logs, want 425: the unsealed stream's entries must have survived truncation", got)
	}
}

func TestStore_AgedHeadsAreSealedByTheClock(t *testing.T) {
	dir := t.TempDir()
	s, clk := openStore(t, dir, func(o *Options) { o.BlockBytes = 1 << 30; o.BlockAge = time.Minute })
	defer func() { _ = s.Close() }()
	_ = s.Append(context.Background(), batchOf("m", 0, 5, t0.UnixMilli()))
	headLen := func() int {
		s.smu.RLock()
		defer s.smu.RUnlock()
		n := 0
		for _, st := range s.byID {
			n += len(st.head)
		}
		return n
	}
	clk.Advance(30 * time.Second)
	time.Sleep(50 * time.Millisecond)
	if headLen() != 5 {
		t.Fatal("a head younger than BlockAge was sealed")
	}
	waitTicker(t, clk)
	clk.Advance(2 * time.Minute)
	waitFor(t, func() bool { return headLen() == 0 }, "an aged head was never sealed")
	if len(findFiles(t, dir, ".chunk")) != 1 {
		t.Fatal("the sealed head is not in a chunk file")
	}
}

func TestStore_ExpiresWholeDaysPastRetention(t *testing.T) {
	dir := t.TempDir()
	s, clk := openStore(t, dir, func(o *Options) { o.Retention = 48 * time.Hour })
	defer func() { _ = s.Close() }()
	old := t0.Add(-5 * 24 * time.Hour).UnixMilli()
	_ = s.Append(context.Background(), batchOf("old", 0, 5, old))
	_ = s.Append(context.Background(), batchOf("new", 0, 5, t0.UnixMilli()))
	_ = s.Flush()
	if got := len(findFiles(t, dir, ".chunk")); got != 2 {
		t.Fatalf("%d chunks before expiry, want 2 (one per day)", got)
	}
	waitTicker(t, clk)
	clk.Advance(2 * time.Second)
	waitFor(t, func() bool { return len(findFiles(t, dir, ".chunk")) == 1 }, "the old day was not deleted")
	got := everything(t, s)
	for _, m := range got {
		if strings.HasPrefix(m, "old") {
			t.Fatalf("an expired log is still searchable: %q", m)
		}
	}
	if len(got) != 5 {
		t.Fatalf("%d logs left, want the 5 from today", len(got))
	}
	// The expired day's directory goes with its last chunk.
	days, _ := os.ReadDir(dir)
	n := 0
	for _, d := range days {
		if d.IsDir() && dayDir.MatchString(d.Name()) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d day directories remain, want 1", n)
	}
}

// waitTicker waits until the store's loop has armed its ticker, so an Advance
// is not missed.
func waitTicker(t testing.TB, clk *testutil.FakeClock) {
	t.Helper()
	waitFor(t, func() bool { return clk.Waiters() >= 1 }, "the store's ticker was never armed")
}

func waitFor(t testing.TB, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
