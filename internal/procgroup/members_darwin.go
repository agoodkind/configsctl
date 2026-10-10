//go:build darwin

package procgroup

import (
	"fmt"
	"log/slog"

	"golang.org/x/sys/unix"
)

const darwinZombieState = 5

func liveMembers(groupID int) (int, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", groupID)
	if err != nil {
		slog.Error("procgroup.members_failed", "group", groupID, "err", err)
		return 0, fmt.Errorf("list process group %d: %w", groupID, err)
	}
	live := 0
	for _, process := range processes {
		if process.Proc.P_stat != darwinZombieState {
			live++
		}
	}
	return live, nil
}
