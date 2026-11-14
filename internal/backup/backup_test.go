package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func made(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "tsdb/blocks/b1/chunks.dat", "chunk-bytes")
	write(t, dir, "tsdb/wal/00000001.wal", "wal-bytes")
	write(t, dir, "meta.db", "sqlite")
	if _, err := Finish(dir, "v1", time.Unix(1790000000, 0)); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRoundTrip_FinishVerifyRestore(t *testing.T) {
	src := made(t)
	m, err := Verify(src)
	if err != nil || len(m.Files) != 3 || m.Version != "v1" {
		t.Fatalf("verify: %+v %v", m, err)
	}
	dst := filepath.Join(t.TempDir(), "data")
	if _, err := Restore(src, dst); err != nil {
		t.Fatal(err)
	}
	if read(t, dst, "tsdb/blocks/b1/chunks.dat") != "chunk-bytes" || read(t, dst, "meta.db") != "sqlite" {
		t.Fatal("restored files differ")
	}
	if _, err := os.Stat(filepath.Join(dst, ManifestName)); err == nil {
		t.Fatal("the manifest was copied into data_dir")
	}
}

// What makes a manifest worth having: each of these must be caught before a byte
// reaches data_dir.
func TestVerify_CatchesEveryWayABackupCanBeWrong(t *testing.T) {
	for name, damage := range map[string]func(dir string){
		"a changed file":     func(d string) { write(t, d, "meta.db", "SQLITE") },
		"a shorter file":     func(d string) { write(t, d, "meta.db", "sql") },
		"a missing file":     func(d string) { _ = os.Remove(filepath.Join(d, "meta.db")) },
		"an extra file":      func(d string) { write(t, d, "tsdb/wal/00000002.wal", "x") },
		"no manifest":        func(d string) { _ = os.Remove(filepath.Join(d, ManifestName)) },
		"a garbled manifest": func(d string) { write(t, d, ManifestName, "{not json") },
		"an escaping path": func(d string) {
			write(t, d, ManifestName, `{"format":1,"files":[{"path":"../x","size":1,"sha256":"00"}]}`)
		},
		"a future format": func(d string) { write(t, d, ManifestName, `{"format":99,"files":[]}`) },
	} {
		src := made(t)
		damage(src)
		if _, err := Verify(src); err == nil {
			t.Errorf("%s: Verify passed", name)
		}
		dst := filepath.Join(t.TempDir(), "data")
		if _, err := Restore(src, dst); err == nil {
			t.Errorf("%s: Restore went ahead", name)
		}
		if es, _ := os.ReadDir(dst); len(es) > 0 {
			t.Errorf("%s: Restore wrote %d entries before it noticed", name, len(es))
		}
	}
}

func TestRestore_RefusesADataDirThatHoldsData(t *testing.T) {
	src := made(t)
	dst := t.TempDir()
	write(t, dst, "meta.db", "precious")
	if _, err := Restore(src, dst); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("err = %v", err)
	}
	if read(t, dst, "meta.db") != "precious" {
		t.Fatal("restore touched existing data")
	}
}

func TestCopyPrefix_CopiesExactlyTheMarkedBytesAndRefusesAShortFile(t *testing.T) {
	d := t.TempDir()
	write(t, d, "a", "0123456789")
	if err := CopyPrefix(filepath.Join(d, "a"), filepath.Join(d, "b"), 4); err != nil || read(t, d, "b") != "0123" {
		t.Fatalf("prefix: %v %q", err, read(t, d, "b"))
	}
	if err := CopyPrefix(filepath.Join(d, "a"), filepath.Join(d, "c"), 99); err == nil {
		t.Fatal("a file shorter than its mark was accepted")
	}
	if _, err := os.Stat(filepath.Join(d, "c")); err == nil {
		t.Fatal("a failed copy left a file behind")
	}
	if err := CopyPrefix(filepath.Join(d, "a"), filepath.Join(d, "e"), -1); err != nil || read(t, d, "e") != "0123456789" {
		t.Fatal("whole copy")
	}
	if err := CopyPrefix(filepath.Join(d, "a"), filepath.Join(d, "e"), -1); err == nil {
		t.Fatal("an existing destination was overwritten")
	}
}

func TestLinkTree_LinksImmutableFilesAndRecreatesTheShape(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	write(t, src, "b1/index.dat", "i")
	write(t, src, "b1/chunks.dat", "c")
	if err := os.MkdirAll(filepath.Join(src, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := LinkTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if read(t, dst, "b1/chunks.dat") != "c" {
		t.Fatal("not linked")
	}
	if _, err := os.Stat(filepath.Join(dst, "empty")); err != nil {
		t.Fatal("an empty directory was not recreated")
	}
	a, _ := os.Stat(filepath.Join(src, "b1/index.dat"))
	b, _ := os.Stat(filepath.Join(dst, "b1/index.dat"))
	if !os.SameFile(a, b) {
		t.Log("not a hard link on this filesystem; copied instead") // allowed, not required
	}
}

func TestPrepareDir_CreatesAndRefusesAFullOne(t *testing.T) {
	d := filepath.Join(t.TempDir(), "a", "b")
	if err := PrepareDir(d); err != nil {
		t.Fatal(err)
	}
	write(t, d, "x", "x")
	if err := PrepareDir(d); err == nil {
		t.Fatal("a non-empty directory was accepted")
	}
}

func TestSummary_SaysWhatIsInTheBackup(t *testing.T) {
	m, err := Verify(made(t))
	if err != nil {
		t.Fatal(err)
	}
	if s := m.Summary(); !strings.Contains(s, "3 files") || !strings.Contains(s, "v1") {
		t.Fatalf("summary %q", s)
	}
}

// The fallbacks: a link that cannot be made falls back to a copy, and when the
// copy cannot be made either (the destination exists) that is an error, not a
// silent skip.
func TestLinkOrCopy_FallsBackToACopyAndReportsAFailure(t *testing.T) {
	d := t.TempDir()
	write(t, d, "src", "bytes")
	if err := LinkOrCopy(filepath.Join(d, "src"), filepath.Join(d, "sub", "dst")); err != nil || read(t, d, "sub/dst") != "bytes" {
		t.Fatalf("link: %v", err)
	}
	if err := LinkOrCopy(filepath.Join(d, "src"), filepath.Join(d, "sub", "dst")); err == nil {
		t.Fatal("an existing destination was overwritten")
	}
	if err := LinkOrCopy(filepath.Join(d, "missing"), filepath.Join(d, "x")); err == nil {
		t.Fatal("a missing source was accepted")
	}
}

func TestPrepareDir_RefusesAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	write(t, filepath.Dir(f), "f", "x")
	if err := PrepareDir(f); err == nil {
		t.Fatal("a regular file was accepted as a directory")
	}
}

func TestFinish_ErrorsOnAMissingDirectory(t *testing.T) {
	if _, err := Finish(filepath.Join(t.TempDir(), "nope"), "v", time.Now()); err == nil {
		t.Fatal("manifest of a directory that does not exist")
	}
}

func TestRestore_AnEmptyBackupRestoresNothingButStillMakesTheDirectory(t *testing.T) {
	src := t.TempDir()
	if _, err := Finish(src, "v1", time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "data")
	if _, err := Restore(src, dst); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dst); err != nil || !fi.IsDir() {
		t.Fatalf("data_dir not created: %v", err)
	}
}

func TestLinkTree_ErrorsOnAMissingSource(t *testing.T) {
	if err := LinkTree(filepath.Join(t.TempDir(), "nope"), t.TempDir()); err == nil {
		t.Fatal("linking a tree that does not exist")
	}
}
