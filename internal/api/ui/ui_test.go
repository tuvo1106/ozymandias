package ui

import (
	"io/fs"
	"testing"
)

// The embed must always contain at least the tracked placeholder, whether or
// not the UI has been built — this is what keeps `go build` Node-free.
func TestFS_ContainsPlaceholderOrBuild(t *testing.T) {
	entries, err := fs.ReadDir(FS(), ".")
	if err != nil || len(entries) == 0 {
		t.Fatalf("embedded UI dir is empty or unreadable: %v", err)
	}
}
