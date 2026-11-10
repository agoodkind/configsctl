//go:build unix

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"syscall"
)

// processRunning reports whether a process with the pid exists. A process of
// another user answers EPERM, which still means the process exists.
func processRunning(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true, nil
	}

	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}

	slog.Error("providers.process_check_failed", "pid", pid, "err", err)

	return false, fmt.Errorf("check process %d: %w", pid, err)
}
