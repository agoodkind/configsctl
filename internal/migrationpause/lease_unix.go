//go:build unix

package migrationpause

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// AcquireWriter excludes migration while an ordinary infrastructure writer runs.
func AcquireWriter(pauseFile string) (func(), error) {
	if err := Guard(pauseFile); err != nil {
		return nil, err
	}
	release, err := acquire(pauseFile, syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	if err := Guard(pauseFile); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// AcquireExclusive excludes ordinary writers during migration or recovery.
func AcquireExclusive(pauseFile string) (func(), error) {
	return acquire(pauseFile, syscall.LOCK_EX)
}

func acquire(pauseFile string, operation int) (func(), error) {
	directory := filepath.Dir(pauseFile)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		slog.Error("migration.lease.directory_failed", "path", directory, "err", err)
		return nil, fmt.Errorf("create the private migration lease directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect the private migration lease directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, errors.New("the migration lease directory must be a real private directory with mode 0700")
	}
	stat, valid := info.Sys().(*syscall.Stat_t)
	user := os.Geteuid()
	if !valid || user < 0 || uint64(stat.Uid) != uint64(user) {
		return nil, errors.New("the current user must own the migration lease directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, fmt.Errorf("open the private migration lease directory: %w", err)
	}
	defer root.Close()
	file, err := root.OpenFile("writers.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the infrastructure writer lease: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), operation|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			slog.Error("migration.lease.acquire_failed", "err", err)
			return nil, fmt.Errorf("acquire the infrastructure writer lease: %w", err)
		}
		if operation&syscall.LOCK_EX != 0 {
			return nil, errors.New("active infrastructure writers or another migration prevent the exclusive lease; wait for those operations to finish")
		}
		return nil, errors.New("an active migration prevents this infrastructure write; wait for it to finish")
	}
	return func() { _ = file.Close() }, nil
}
