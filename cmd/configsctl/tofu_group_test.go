//go:build unix

package main_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const lingeringChildWait = 10 * time.Second

const fakeTofuScript = `#!/bin/sh
sleep 300 </dev/null >/dev/null 2>&1 &
echo $! > '%s'
exit %d
`

func installFakeTofu(t *testing.T, exitCode int) (string, string) {
	t.Helper()
	binDir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := fmt.Sprintf(fakeTofuScript, pidFile, exitCode)
	if err := os.WriteFile(filepath.Join(binDir, "tofu"), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake tofu: %v", err)
	}
	return binDir + string(os.PathListSeparator) + os.Getenv("PATH"), pidFile
}

func readChildPID(t *testing.T, pidFile string) int {
	t.Helper()
	body, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read the fake tofu child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatalf("parse the fake tofu child pid %q: %v", body, err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return pid
}

func requireProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(lingeringChildWait)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake tofu child %d survived configsctl: kill 0 = %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTofuCrashStopsLingeringChildAndReportsTheCrash(t *testing.T) {
	tree := newConfigsTree(t)
	path, pidFile := installFakeTofu(t, 11)

	result := runConfigsctlWithPath(t, tree, path, "tofu", "alpha", "plan")
	child := readChildPID(t, pidFile)

	if result.exitCode == 0 {
		t.Fatalf("configsctl tofu alpha plan succeeded after tofu exited 11\nstderr: %s", result.stderr)
	}
	if !strings.Contains(result.stderr, "OpenTofu crashed") {
		t.Fatalf("stderr = %q, want it to report that OpenTofu crashed", result.stderr)
	}
	logDir := filepath.Join(tree.root, "tmp", "configs-runs")
	if !strings.Contains(result.stderr, logDir) {
		t.Fatalf("stderr = %q, want it to name the run log under %s", result.stderr, logDir)
	}
	requireProcessGone(t, child)
}

func TestTofuSuccessStopsLingeringChild(t *testing.T) {
	tree := newConfigsTree(t)
	path, pidFile := installFakeTofu(t, 0)

	args := []string{"tofu", "alpha", "plan"}
	result := runConfigsctlWithPath(t, tree, path, args...)
	child := readChildPID(t, pidFile)

	requireSuccess(t, result, args...)
	requireProcessGone(t, child)
}
