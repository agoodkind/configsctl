//go:build linux

package procgroup_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const (
	procStateZombie = "Z"
	procStateDead   = "X"
)

func processStopped(pid int) (bool, error) {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true, nil
	}
	statPath := "/proc/" + strconv.Itoa(pid) + "/stat"
	stat, err := os.ReadFile(statPath)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", statPath, err)
	}
	commandEnd := bytes.LastIndexByte(stat, ')')
	if commandEnd < 0 {
		return false, fmt.Errorf("%s has no command name: %q", statPath, stat)
	}
	fields := strings.Fields(string(stat[commandEnd+1:]))
	if len(fields) == 0 {
		return false, fmt.Errorf("%s has no state field: %q", statPath, stat)
	}
	state := fields[0]
	return state == procStateZombie || state == procStateDead, nil
}
