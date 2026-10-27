//go:build unix

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
	"syscall"
	"testing"
	"time"

	"goodkind.io/configsctl/internal/procgroup"
)

const signalPlaybook = `- name: Run the signal fixture
  hosts: all
  gather_facts: false
  tasks: []
`

const (
	signalPlaybookName = "deploy-signal"
	signalHost         = "signal-host"
	signalInventory    = "all:\n  hosts:\n    " + signalHost + ":\n"
	signalLockFile     = "deploy.lock"
	signalStartWait    = time.Minute
	signalExitWait     = procgroup.StopGrace + 30*time.Second
	signalPollInterval = 20 * time.Millisecond
)

// signalRun is one configsctl process that runs a stand-in child.
type signalRun struct {
	command *exec.Cmd
	exited  chan error
	stderr  *bytes.Buffer
	standIn int
	worker  int
}

// startSignalRun starts configsctl in a new session with the named stand-ins
// first on PATH and waits until the stand-in child writes its process IDs.
// A new session has no controlling terminal, as under systemd, so configsctl
// starts the child in its own process group.
func startSignalRun(t *testing.T, tree configsTree, standIns []string, extraEnv []string, args ...string) signalRun {
	t.Helper()
	binDir := t.TempDir()
	for _, name := range standIns {
		script, err := os.ReadFile(filepath.Join("testdata", "signalstop", name))
		if err != nil {
			t.Fatalf("read the %s stand-in: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(binDir, name), script, 0o700); err != nil {
			t.Fatalf("install the %s stand-in: %v", name, err)
		}
	}
	tempDir, err := os.MkdirTemp(shortTempRoot, "cfgsig")
	if err != nil {
		t.Fatalf("create a short TMPDIR: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })
	pidFile := filepath.Join(t.TempDir(), "child.pids")
	ctx, cancel := context.WithTimeout(t.Context(), signalStartWait+signalExitWait)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, configsctlBinary, args...)
	command.Dir = tree.root
	command.Env = append([]string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + tree.home,
		"TMPDIR=" + tempDir,
		"FIXTURE_PIDS=" + pidFile,
	}, extraEnv...)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	run := signalRun{command: command, exited: make(chan error, 1), stderr: &bytes.Buffer{}, standIn: 0, worker: 0}
	command.Stdout = &bytes.Buffer{}
	command.Stderr = run.stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start configsctl %v: %v", args, err)
	}
	go func() { run.exited <- command.Wait() }()
	run.standIn, run.worker = waitForStandIn(t, pidFile, run)
	t.Cleanup(func() { _ = syscall.Kill(-run.standIn, syscall.SIGKILL) })
	if err := syscall.Kill(-run.standIn, 0); err != nil {
		t.Fatalf("the stand-in %d does not lead its own process group: %v", run.standIn, err)
	}
	return run
}

func waitForStandIn(t *testing.T, path string, run signalRun) (int, int) {
	t.Helper()
	deadline := time.NewTimer(signalStartWait)
	defer deadline.Stop()
	ticker := time.NewTicker(signalPollInterval)
	defer ticker.Stop()
	for {
		if body, err := os.ReadFile(path); err == nil {
			fields := strings.Fields(string(body))
			if len(fields) != 2 {
				t.Fatalf("the stand-in wrote %q, want two process IDs", body)
			}
			standIn, standInErr := strconv.Atoi(fields[0])
			worker, workerErr := strconv.Atoi(fields[1])
			if standInErr != nil || workerErr != nil {
				t.Fatalf("parse the stand-in process IDs %q: %v", body, errors.Join(standInErr, workerErr))
			}
			return standIn, worker
		}
		select {
		case err := <-run.exited:
			t.Fatalf("configsctl exited before the stand-in started: %v\nstderr: %s", err, run.stderr.String())
		case <-deadline.C:
			t.Fatalf("the stand-in did not start within %s", signalStartWait)
		case <-ticker.C:
		}
	}
}

// stopSignalRun sends SIGTERM to configsctl and requires exit status 1, the
// wanted error text, and no running stand-in process.
func stopSignalRun(t *testing.T, run signalRun, wantError string) {
	t.Helper()
	if err := run.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM to configsctl: %v", err)
	}
	var waitErr error
	select {
	case waitErr = <-run.exited:
	case <-time.After(signalExitWait):
		t.Fatalf("configsctl did not exit within %s of SIGTERM", signalExitWait)
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("configsctl returned %v after SIGTERM, want exit status 1\nstderr: %s", waitErr, run.stderr.String())
	}
	if !strings.Contains(run.stderr.String(), wantError) {
		t.Fatalf("stderr = %q, want %q", run.stderr.String(), wantError)
	}
	requireProcessGone(t, run.standIn)
	requireProcessGone(t, run.worker)
}

func TestDeployStopsThePlayAndReleasesTheLockOnSIGTERM(t *testing.T) {
	if _, err := exec.LookPath("ansible-inventory"); err != nil {
		t.Skip("ansible-inventory is not installed")
	}
	tree := newConfigsTree(t)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "ansible.cfg"), lockedAnsibleConfig)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "inventory", "hosts.yml"), signalInventory)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "playbooks", signalPlaybookName+".yml"), signalPlaybook)
	lockDir := t.TempDir()
	extraEnv := []string{"FIXTURE_PLAY_HOST=" + signalHost, "FIXTURE_LOCK_DIR=" + lockDir}

	run := startSignalRun(t, tree, []string{"ansible-playbook", "ssh"}, extraEnv, "deploy", signalPlaybookName)
	requireFile(t, filepath.Join(lockDir, signalLockFile))
	stopSignalRun(t, run, "deploy "+signalPlaybookName+" stopped on a signal: terminated signal received")
	requireNoFile(t, filepath.Join(lockDir, signalLockFile))
}

func TestTofuStopsItsProcessGroupOnSIGTERM(t *testing.T) {
	tree := newConfigsTree(t)

	run := startSignalRun(t, tree, []string{"tofu"}, nil, "tofu", "alpha", "plan")
	stopSignalRun(t, run, "tofu stopped: terminated signal received")
}
