package hostlock_test

import (
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// bootID returns the boot id that the lock script puts in its guard path.
func bootID(t *testing.T) string {
	t.Helper()
	booted, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		t.Fatalf("read kern.boottime: %v", err)
	}
	return strconv.FormatInt(booted.Sec, 10)
}
