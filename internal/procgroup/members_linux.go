//go:build linux

package procgroup

import (
	"bytes"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

const (
	procStateField = 0
	procGroupField = 2
)

func liveMembers(groupID int) (int, error) {
	proc, err := os.OpenRoot("/proc")
	if err != nil {
		slog.Error("procgroup.members_failed", "group", groupID, "err", err)
		return 0, fmt.Errorf("open /proc: %w", err)
	}
	defer func() { _ = proc.Close() }()
	entries, err := fs.ReadDir(proc.FS(), ".")
	if err != nil {
		slog.Error("procgroup.members_failed", "group", groupID, "err", err)
		return 0, fmt.Errorf("list /proc: %w", err)
	}
	live := 0
	for _, entry := range entries {
		if _, convErr := strconv.Atoi(entry.Name()); convErr != nil {
			continue
		}
		stat, readErr := proc.ReadFile(entry.Name() + "/stat")
		if readErr == nil && isLiveMember(stat, groupID) {
			live++
		}
	}
	return live, nil
}

func isLiveMember(stat []byte, groupID int) bool {
	commandEnd := bytes.LastIndexByte(stat, ')')
	if commandEnd < 0 {
		return false
	}
	fields := strings.Fields(string(stat[commandEnd+1:]))
	if len(fields) <= procGroupField {
		return false
	}
	group, err := strconv.Atoi(fields[procGroupField])
	if err != nil || group != groupID {
		return false
	}
	state := fields[procStateField]
	return state != "Z" && state != "X"
}
