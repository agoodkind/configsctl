//go:build unix

// Package fileowner reports whether the current user owns a file system entry.
package fileowner

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// CurrentUser reports whether the user running this process owns the entry.
// Other local users can write to some host temp directories. Ownership checks
// distinguish the operator's run log directory from another user's entry.
func CurrentUser(info fs.FileInfo) (bool, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, errors.New("this filesystem does not report a file owner")
	}
	return int(stat.Uid) == os.Getuid(), nil
}
