//go:build unix

package tailer

import (
	"fmt"
	"os"
	"syscall"
)

// fileID is a file's identity: device and inode, which survive a rename and
// are not reused while the file is open. A path is not an identity: the path
// of a rotated log is soon a different file.
type fileID struct{ dev, ino uint64 }

func (i fileID) String() string     { return fmt.Sprintf("%d:%d", i.dev, i.ino) }
func (i fileID) less(o fileID) bool { return i.dev < o.dev || (i.dev == o.dev && i.ino < o.ino) }

func idOf(fi os.FileInfo) (fileID, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, false
	}
	return fileID{uint64(st.Dev), uint64(st.Ino)}, true //nolint:unconvert // widths differ by platform
}
