//go:build unix

package main_test

import (
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
sleep 300 </dev/null %s &
echo $! > '%s'
exit %d
`

const (
	childOutputDiscarded = ">/dev/null 2>&1"
	childOutputInherited = ""
)

const inheritedOutputExitLimit = 5 * time.Second

func installFakeTofu(t *testing.T, exitCode int, childOutput string) (string, string) {
	t.Helper()
	binDir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := fmt.Sprintf(fakeTofuScript, childOutput, pidFile, exitCode)
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
		stopped, err := processStopped(pid)
		if stopped {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake tofu child %d survived configsctl: state check error = %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTofuCrashStopsLingeringChildAndReportsTheCrash(t *testing.T) {
	tree := newConfigsTree(t)
	path, pidFile := installFakeTofu(t, 11, childOutputDiscarded)

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
	path, pidFile := installFakeTofu(t, 0, childOutputDiscarded)

	args := []string{"tofu", "alpha", "plan"}
	result := runConfigsctlWithPath(t, tree, path, args...)
	child := readChildPID(t, pidFile)

	requireSuccess(t, result, args...)
	requireProcessGone(t, child)
}

func TestTofuSuccessReturnsPromptlyWhenChildKeepsTheRunLogOpen(t *testing.T) {
	tree := newConfigsTree(t)
	path, pidFile := installFakeTofu(t, 0, childOutputInherited)

	args := []string{"tofu", "alpha", "plan"}
	started := time.Now()
	result := runConfigsctlWithPath(t, tree, path, args...)
	elapsed := time.Since(started)
	child := readChildPID(t, pidFile)

	requireSuccess(t, result, args...)
	if elapsed > inheritedOutputExitLimit {
		t.Fatalf("configsctl took %v to return, want under %v", elapsed, inheritedOutputExitLimit)
	}
	requireProcessGone(t, child)
}
