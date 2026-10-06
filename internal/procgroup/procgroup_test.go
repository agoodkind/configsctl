//go:build unix

package procgroup_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"goodkind.io/configsctl/internal/procgroup"
)

const pidWait = 10 * time.Second

func TestRunStopsTheDescendantsOfACanceledCommand(t *testing.T) {
	if tty, err := os.Open("/dev/tty"); err == nil {
		_ = tty.Close()
		t.Skip("this process has a controlling terminal; the controller runs without one")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", `sleep 300 & echo $! > "$1"; wait`, "bash", pidFile)
	done := make(chan error, 1)
	go func() { done <- procgroup.Run(cmd) }()
	child := waitForPID(t, pidFile)
	cancel()
	select {
	case <-done:
	case <-time.After(procgroup.StopGrace + pidWait):
		t.Fatal("Run did not return after the cancel")
	}
	// The init process reaps the killed child after Run returns. A zombie
	// still answers kill 0 until then.
	deadline := time.Now().Add(pidWait)
	for {
		err := syscall.Kill(child, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the background child %d survived Run: kill 0 = %v", child, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(pidWait)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil && strings.HasSuffix(string(body), "\n") {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(body)))
			if convErr != nil {
				t.Fatalf("read the child pid: %v", convErr)
			}
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the command did not write %s", path)
	return 0
}
