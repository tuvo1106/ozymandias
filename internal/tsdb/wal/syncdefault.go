//go:build !faultinject

package wal

import "os"

// defaultSyncFile is the fsync a log uses when its Options name none: the file's own. A build with
// the faultinject tag selects a different one (faultinject.go), which the slow-disk chaos drill uses;
// this is the one every other build has, and it reads nothing from the environment.
func defaultSyncFile() func(*os.File) error { return (*os.File).Sync }
