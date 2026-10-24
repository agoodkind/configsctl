//go:build unix

package statemigration

import (
	"errors"
	"os"
	"syscall"
)

func requirePrivateOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	uid := os.Geteuid()
	if !ok || uid < 0 || uint64(stat.Uid) != uint64(uid) {
		return errors.New("the current user must own the migration directory")
	}
	return nil
}

func lockExecutor(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another process owns this migration executor; wait for it to finish")
	}
	return nil
}
