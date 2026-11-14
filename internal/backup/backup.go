package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cockroachdb/pebble"
)

// ManifestName is the manifest's file name at the backup's root.
const ManifestName = "MANIFEST.json"

// formatVersion is bumped when the layout or the manifest changes shape.
const formatVersion = 1

// File is one file in a backup.
type File struct {
	Path   string `json:"path"` // slash-separated, relative to the backup root
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest vouches for every file of a backup.
type Manifest struct {
	Format    int       `json:"format"`
	CreatedAt time.Time `json:"created_at"`
	// Version is the ozyd that wrote it.
	Version string `json:"version"`
	Files   []File `json:"files"`
}

// ErrNotEmpty is returned for a destination that already holds something.
var ErrNotEmpty = errors.New("backup: the directory is not empty")

// PrepareDir creates dir (and parents) and requires it to be empty. A backup
// into a directory that already has files would mix two snapshots, and a
// restore into a data_dir that already has data would mix two histories, so
// both refuse rather than merge.
func PrepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	es, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if len(es) > 0 {
		return fmt.Errorf("%w: %s", ErrNotEmpty, dir)
	}
	return nil
}

// CopyPrefix copies the first n bytes of src to dst (all of it if n < 0) and
// fsyncs it. A file that is only appended to has a prefix that never changes,
// so a copy of the prefix known to be valid is a consistent copy of a file that
// is still growing.
func CopyPrefix(src, dst string, n int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	var r io.Reader = in
	if n >= 0 {
		r = io.LimitReader(in, n)
	}
	copied, err := io.Copy(out, r)
	if err == nil && n >= 0 && copied < n {
		err = fmt.Errorf("%s is %d bytes, shorter than the %d the backup needs", src, copied, n)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("backup: copying %s: %w", src, err)
	}
	return nil
}

// LinkOrCopy hard-links src at dst, or copies it when a link is not possible
// (another filesystem). Only for files that are never modified in place.
func LinkOrCopy(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return CopyPrefix(src, dst, -1)
}

// LinkTree applies LinkOrCopy to every regular file under src, into dst.
func LinkTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return LinkOrCopy(path, filepath.Join(dst, rel))
	})
}

// PebbleCheckpoint writes a consistent copy of a live Pebble database to dst, which
// must not exist. The log is flushed first (WithFlushedWAL): without it, writes
// acknowledged without an fsync are missing from the copy. A checkpoint hard-links
// the immutable tables (copying across filesystems) and takes the log and manifest
// at one instant, which a plain copy of a live directory cannot, because background
// compactions rewrite files while it runs.
func PebbleCheckpoint(db *pebble.DB, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return db.Checkpoint(dst, pebble.WithFlushedWAL())
}

// Finish walks dir, hashes every file and writes the manifest. It is the last
// step, so the manifest describes exactly what is there.
func Finish(dir, version string, now time.Time) (Manifest, error) {
	m := Manifest{Format: formatVersion, CreatedAt: now.UTC(), Version: version}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == ManifestName {
			return nil
		}
		sum, size, err := hashFile(path)
		if err != nil {
			return err
		}
		m.Files = append(m.Files, File{Path: filepath.ToSlash(rel), Size: size, SHA256: sum})
		return nil
	})
	if err != nil {
		return m, fmt.Errorf("backup: %w", err)
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return m, err
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName), append(b, '\n'), 0o644); err != nil {
		return m, fmt.Errorf("backup: %w", err)
	}
	if err := syncTree(dir); err != nil {
		return m, err
	}
	return m, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// Verify checks dir against its manifest: every listed file present with the
// recorded size and hash, and nothing there that the manifest does not list.
// An extra file is an error because a backup is a closed set; restoring one
// that has acquired a file would put data in data_dir that nothing vouched for.
func Verify(dir string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return m, fmt.Errorf("backup: no manifest in %s: %w", dir, err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("backup: unreadable manifest: %w", err)
	}
	if m.Format != formatVersion {
		return m, fmt.Errorf("backup: manifest format %d, this ozyd reads %d", m.Format, formatVersion)
	}
	listed := map[string]bool{}
	var errs []error
	for _, f := range m.Files {
		if !fs.ValidPath(f.Path) {
			return m, fmt.Errorf("backup: manifest path %q escapes the backup", f.Path)
		}
		listed[f.Path] = true
		sum, size, err := hashFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", f.Path, err))
		case size != f.Size || sum != f.SHA256:
			errs = append(errs, fmt.Errorf("%s: changed since the backup was made (size %d, want %d)", f.Path, size, f.Size))
		}
	}
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		if rel != ManifestName && !listed[rel] {
			errs = append(errs, fmt.Errorf("%s: not in the manifest", rel))
		}
		return nil
	}); err != nil {
		errs = append(errs, fmt.Errorf("scanning for files the manifest does not list: %w", err))
	}
	if len(errs) > 0 {
		return m, fmt.Errorf("backup: %s does not match its manifest: %w", dir, errors.Join(errs...))
	}
	return m, nil
}

// Restore verifies the backup in src and copies it into dataDir, which must be
// empty (or absent). It copies, never links: a restored data_dir that shared
// files with the backup would modify the backup the first time ozyd wrote to it.
func Restore(src, dataDir string) (Manifest, error) {
	m, err := Verify(src)
	if err != nil {
		return m, err
	}
	if err := PrepareDir(dataDir); err != nil {
		return m, err
	}
	for _, f := range m.Files {
		if err := CopyPrefix(filepath.Join(src, filepath.FromSlash(f.Path)),
			filepath.Join(dataDir, filepath.FromSlash(f.Path)), -1); err != nil {
			return m, err
		}
	}
	// Directories that exist only as structure (an empty chunk dir) are not in
	// the manifest and are recreated by the stores that need them.
	return m, syncTree(dataDir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

func syncTree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return syncDir(path)
		}
		return nil
	})
}

// Summary is a one-line description of a manifest for logs and the CLI.
func (m Manifest) Summary() string {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return fmt.Sprintf("%d files, %d bytes, written by %s at %s", len(m.Files), n, strings.TrimSpace(m.Version), m.CreatedAt.Format(time.RFC3339))
}
