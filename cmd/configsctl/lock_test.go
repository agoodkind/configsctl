package main_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/configsctl/hostlock"
)

const (
	lockRun          = "20261005T050000Z-aaaaaaaa"
	otherLockRun     = "20261005T050100Z-bbbbbbbb"
	lockedHostName   = "gateway"
	extendSeconds    = 600
	extendStartSlack = 30 * time.Second
)

func runLockCommand(t *testing.T, stateDir string, args ...string) commandResult {
	t.Helper()
	workDir := t.TempDir()
	command := exec.CommandContext(context.Background(), configsctlBinary, args...)
	command.Dir = workDir
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + workDir,
		"TMPDIR=" + workDir,
	}
	if stateDir != "" {
		command.Env = append(command.Env, hostlock.StateEnvVar+"="+stateDir)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()
	result := commandResult{stdout: stdout.String(), stderr: stderr.String(), exitCode: 0}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		result.exitCode = exitErr.ExitCode()
	default:
		t.Fatalf("run configsctl %v: %v", args, runErr)
	}
	return result
}

func lockLocalHost(t *testing.T) (hostlock.Host, *hostlock.Set) {
	t.Helper()
	host := hostlock.Host{Name: lockedHostName, User: "", Address: "", Dir: t.TempDir()}
	set, err := hostlock.AcquireAll(t.Context(), []hostlock.Host{host}, lockRun, "controller-a", hostlock.DefaultLease())
	if err != nil {
		t.Fatalf("AcquireAll: %v", err)
	}
	releaseCtx := context.WithoutCancel(t.Context())
	t.Cleanup(func() { set.ReleaseAll(releaseCtx) })
	return host, set
}

func TestLockExtendDeclaresALongerLeaseForTheRunningDeploy(t *testing.T) {
	host, set := lockLocalHost(t)
	started := time.Now()

	args := []string{"lock", "extend", "--host", lockedHostName, "--seconds", strconv.Itoa(extendSeconds)}
	result := runLockCommand(t, set.StateDir(), args...)
	requireSuccess(t, result, args...)

	err := hostlock.Do(t.Context(), host, hostlock.Acquire, otherLockRun, "controller-b")
	var held *hostlock.HeldError
	if !errors.As(err, &held) || held.Run != lockRun {
		t.Fatalf("acquire by a second run: err = %v, want the lock of %s", err, lockRun)
	}
	earliest := started.Add(extendSeconds*time.Second - time.Second)
	latest := started.Add(extendSeconds*time.Second + extendStartSlack)
	if held.Expiry.Before(earliest) || held.Expiry.After(latest) {
		t.Fatalf("expiry after lock extend = %s, want %d seconds after %s", held.Expiry, extendSeconds, started)
	}
	if want := held.Expiry.UTC().Format(time.RFC3339); !strings.Contains(result.stdout, want) {
		t.Fatalf("stdout = %q, want the expiry %s", result.stdout, want)
	}
	if err := set.RenewAll(t.Context()); err != nil {
		t.Fatalf("RenewAll after lock extend: %v", err)
	}
}

func TestLockExtendRefusesAHostThatTheDeployDidNotLock(t *testing.T) {
	host, set := lockLocalHost(t)
	lockFile := filepath.Join(host.Dir, "deploy.lock")
	acquired, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatalf("read the lock after the acquire: %v", err)
	}

	result := runLockCommand(t, set.StateDir(), "lock", "extend", "--host", "other", "--seconds", "600")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "has no lock on host other") {
		t.Fatalf("lock extend for an unlocked host: exit %d, stderr = %q", result.exitCode, result.stderr)
	}
	result = runLockCommand(t, set.StateDir(), "lock", "extend", "--host="+lockedHostName, "--seconds=901")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "outside 1 to 900 seconds") {
		t.Fatalf("lock extend above the cap: exit %d, stderr = %q", result.exitCode, result.stderr)
	}
	result = runLockCommand(t, "", "lock", "extend", "--host", lockedHostName, "--seconds", "600")
	if result.exitCode == 0 || !strings.Contains(result.stderr, hostlock.StateEnvVar) {
		t.Fatalf("lock extend outside a deploy: exit %d, stderr = %q", result.exitCode, result.stderr)
	}
	body, err := os.ReadFile(lockFile)
	if err != nil || string(body) != string(acquired) {
		t.Fatalf("lock file = %q, err = %v; want %q", body, err, acquired)
	}
}
