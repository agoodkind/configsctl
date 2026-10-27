package hostlock_test

import (
	"os"
	"strings"
	"testing"
)

// bootID returns the boot id that the lock script puts in its guard path.
func bootID(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatalf("read the boot id: %v", err)
	}
	return strings.TrimSpace(string(body))
}
