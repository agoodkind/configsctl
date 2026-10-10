//go:build unix && !linux

package procgroup_test

import (
	"errors"
	"fmt"
	"syscall"
)

func processStopped(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("kill 0 on %d: %w", pid, err)
	}
	return false, nil
}
