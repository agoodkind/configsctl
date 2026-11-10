//go:build unix

package main_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	signalWait = 30 * time.Second
	exitWait   = 10 * time.Second
	pollWait   = 20 * time.Millisecond
)

// blockingProxy accepts the CONNECT request of gh and never answers it, which
// leaves the gh download waiting on its network connection.
type blockingProxy struct {
	server  *httptest.Server
	started chan struct{}
}

func newBlockingProxy(t *testing.T) *blockingProxy {
	t.Helper()
	proxy := &blockingProxy{started: make(chan struct{}, 1)}
	proxy.server = httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case proxy.started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	t.Cleanup(proxy.server.Close)
	return proxy
}

// childProcessIDs returns the direct child processes of a process.
func childProcessIDs(t *testing.T, parent int) []int {
	t.Helper()
	output, err := exec.CommandContext(context.Background(), "pgrep", "-P", strconv.Itoa(parent)).Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil
	}
	if err != nil {
		t.Fatalf("pgrep -P %d: %v", parent, err)
	}
	var children []int
	for _, field := range strings.Fields(string(output)) {
		child, convErr := strconv.Atoi(field)
		if convErr != nil {
			t.Fatal(convErr)
		}
		children = append(children, child)
	}
	return children
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// requireProcessesGone waits for each process to exit.
func requireProcessesGone(t *testing.T, pids []int) {
	t.Helper()
	deadline := time.Now().Add(exitWait)
	for _, pid := range pids {
		for processExists(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("process %d is still running", pid)
			}
			time.Sleep(pollWait)
		}
	}
}

// killProcessTree ends gh and its process group. gh has its own group only
// when the installer has no controlling terminal. The call signals both forms.
func killProcessTree(pids []int) {
	for _, pid := range pids {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

type stalledInstall struct {
	fixture    installFixture
	release    providerRelease
	process    *os.Process
	downloader []int
	wait       func() commandResult
}

// startStalledInstall starts an install with an empty archive cache and the
// real gh executable. gh sends its API request to a proxy that never answers.
// The install then waits in its download. Wait gives up on the output copies
// after exitWait. A surviving gh process cannot block the test longer.
func startStalledInstall(t *testing.T) stalledInstall {
	t.Helper()
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh is not installed: the cancellation test needs the real gh executable")
	}
	proxy := newBlockingProxy(t)
	fixture := newInstallFixture(t)
	fixture.extraEnvironment = []string{
		"GH_TOKEN=fixture-token", "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1",
		"HTTPS_PROXY=" + proxy.server.URL, "https_proxy=" + proxy.server.URL,
	}
	release := newRelease(t, "tag-one", assignedVersion, "binary-one")
	pin := fixture.pin(t, "providers.pin", release.pinLine(hostPlatformName()))
	command := fixture.installCommand(pin)
	command.WaitDelay = exitWait
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		killProcessTree(childProcessIDs(t, command.Process.Pid))
		_ = command.Process.Kill()
	})
	select {
	case <-proxy.started:
	case <-time.After(signalWait):
		t.Fatal("gh did not contact the proxy")
	}
	downloader := childProcessIDs(t, command.Process.Pid)
	if len(downloader) == 0 {
		t.Fatal("the install has no gh subprocess")
	}
	return stalledInstall{
		fixture: fixture, release: release, process: command.Process, downloader: downloader,
		wait: func() commandResult { return commandOutcome(t, command.Wait(), &stdout, &stderr) },
	}
}

func requireEntryCount(t *testing.T, root string, want int) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != want {
		t.Fatalf("%s has %d entries, want %d", root, len(entries), want)
	}
}

func TestProvidersInstallRemovesTemporaryFilesAfterATerminationSignal(t *testing.T) {
	for name, signal := range map[string]os.Signal{"SIGTERM": syscall.SIGTERM, "SIGINT": os.Interrupt} {
		t.Run(name, func(t *testing.T) {
			stalled := startStalledInstall(t)
			if err := stalled.process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			result := stalled.wait()
			if result.exitCode == 0 {
				t.Fatalf("the interrupted install exited 0: %+v", result)
			}
			requireProcessesGone(t, stalled.downloader)
			requireStagingEmpty(t, stalled.fixture)
			requireArchiveStagingEmpty(t, stalled.fixture)
			if got := mirrorSnapshot(t, stalled.fixture.mirror); got != "absent" {
				t.Fatalf("mirror after an interrupted install = %q, want absent", got)
			}
			requireNoFile(t, stalled.fixture.archivePath(stalled.release))
		})
	}
}

func TestProvidersInstallRecoversFromAKilledRun(t *testing.T) {
	stalled := startStalledInstall(t)
	fixture := stalled.fixture
	if err := stalled.process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// SIGKILL skips the installer cleanup and leaves gh running. End gh before
	// Wait, which otherwise waits for the output copies of the surviving gh.
	killProcessTree(stalled.downloader)
	if result := stalled.wait(); result.exitCode == 0 {
		t.Fatalf("the killed install exited 0: %+v", result)
	}
	requireProcessesGone(t, stalled.downloader)
	requireEntryCount(t, fixture.stagingRoot(), 1)
	requireEntryCount(t, fixture.archiveStagingRoot(), 1)
	if got := mirrorSnapshot(t, fixture.mirror); got != "absent" {
		t.Fatalf("mirror after a killed install = %q, want absent", got)
	}
	requireNoFile(t, fixture.archivePath(stalled.release))

	// A live owner and an unrelated entry in each staging root stay.
	var kept []string
	for _, root := range []string{fixture.stagingRoot(), fixture.archiveStagingRoot()} {
		kept = append(kept, filepath.Join(root, "run."+strconv.Itoa(os.Getpid())+".live"), filepath.Join(root, "operator-notes"))
	}
	for _, directory := range kept {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fixture.extraEnvironment = nil
	fixture.cacheArchive(t, stalled.release)
	pin := fixture.pin(t, "providers.pin", stalled.release.pinLine(hostPlatformName()))
	result := fixture.install(t, pin)
	requireSuccess(t, result, "providers", "install")
	requireEntryCount(t, fixture.stagingRoot(), 2)
	requireEntryCount(t, fixture.archiveStagingRoot(), 2)
	for _, directory := range kept {
		requireFile(t, directory)
	}
	requireFile(t, fixture.target(assignedVersion))
}
