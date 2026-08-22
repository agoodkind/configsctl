//go:build unix

package main

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// ownedByCurrentUser reports whether the entry belongs to the user running this
// process. The run log directory sits in a temp directory that other local
// users can write to, so ownership is what separates a directory this tool
// created from one somebody else left under the same name.
func ownedByCurrentUser(info fs.FileInfo) (bool, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, errors.New("this filesystem does not report a file owner")
	}
	return int(stat.Uid) == os.Getuid(), nil
}
