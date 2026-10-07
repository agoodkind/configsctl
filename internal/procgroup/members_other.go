//go:build unix && !darwin && !linux

package procgroup

import (
	"errors"
	"syscall"
)

func liveMembers(groupID int) (int, error) {
	if errors.Is(syscall.Kill(-groupID, 0), syscall.ESRCH) {
		return 0, nil
	}
	return 1, nil
}
