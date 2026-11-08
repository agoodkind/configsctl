//go:build linux

package main_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const (
	helperModeVariable      = "CONFIGS_NETWORK_WATCH_TEST_HELPER"
	helperAddressVariable   = "CONFIGS_NETWORK_WATCH_TEST_ADDRESS"
	helperInterfaceVariable = "CONFIGS_NETWORK_WATCH_TEST_INTERFACE"

	helperUDP4       = "udp4"
	helperUDP6Device = "udp6-device"
	helperIdle       = "idle"
	helperExit       = "exit"

	pollStep        = 200 * time.Millisecond
	processStopWait = 15 * time.Second

	namespaceMountDirectory = "/run/netns"
	standardDescriptorCount = 3
)

var (
	buildOnce   sync.Once
	buildDir    string
	buildBinary string
	errBuild    error
)

func TestMain(m *testing.M) {
	mode := os.Getenv(helperModeVariable)
	if mode != "" {
		os.Exit(runHelper(mode))
	}
	status := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(status)
}

// runHelper runs one test helper process that systemd starts as a unit.
func runHelper(mode string) int {
	address := os.Getenv(helperAddressVariable)
	interfaceName := os.Getenv(helperInterfaceVariable)
	var connection net.PacketConn
	var err error
	switch mode {
	case helperExit:
		return 0
	case helperIdle:
	case helperUDP4:
		connection, err = net.ListenPacket("udp4", address)
	case helperUDP6Device:
		listenConfig := net.ListenConfig{
			Control: func(_, _ string, rawConn syscall.RawConn) error {
				var bindErr error
				controlErr := rawConn.Control(func(descriptor uintptr) {
					bindErr = unix.SetsockoptString(
						int(descriptor), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, interfaceName)
				})
				return errors.Join(controlErr, bindErr)
			},
		}
		connection, err = listenConfig.ListenPacket(context.Background(), "udp6", address)
	default:
		err = fmt.Errorf("unknown helper mode %q", mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	<-signals
	if connection != nil {
		_ = connection.Close()
	}
	return 0
}

// commandBinary builds configs-network-watch once and returns its path.
func commandBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		goTool, err := exec.LookPath("go")
		if err != nil {
			errBuild = fmt.Errorf("find the go tool: %w", err)
			return
		}
		buildDir, err = os.MkdirTemp("", "configs-network-watch-test-")
		if err != nil {
			errBuild = fmt.Errorf("create the build directory: %w", err)
			return
		}
		output := filepath.Join(buildDir, "configs-network-watch")
		build := exec.Command(goTool, "build", "-o", output, ".")
		combined, err := build.CombinedOutput()
		if err != nil {
			errBuild = fmt.Errorf("go build: %w\n%s", err, combined)
			return
		}
		buildBinary = output
	})
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	return buildBinary
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("this test creates network namespaces and requires root")
	}
}

// inNamespace runs operation on a thread that joined namespace. The
// goroutine never unlocks that thread, and the runtime discards a locked
// thread after its goroutine ends.
func inNamespace(namespace netns.NsHandle, operation func() error) error {
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := netns.Set(namespace); err != nil {
			result <- fmt.Errorf("join network namespace: %w", err)
			return
		}
		result <- operation()
	}()
	return <-result
}

// newNamespace creates a network namespace and mounts it when name is nonempty.
func newNamespace(t *testing.T, name string) netns.NsHandle {
	t.Helper()
	mountpoint := ""
	if name != "" {
		mountpoint = filepath.Join(namespaceMountDirectory, name)
		_, err := os.Lstat(mountpoint)
		if err == nil {
			t.Fatalf("network namespace mountpoint %s already exists", mountpoint)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("check network namespace mountpoint %s: %v", mountpoint, err)
		}
	}
	created := make(chan netns.NsHandle, 1)
	failed := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		var namespace netns.NsHandle
		var err error
		if name == "" {
			namespace, err = netns.New()
		} else {
			namespace, err = netns.NewNamed(name)
		}
		if err != nil {
			failed <- err
			return
		}
		created <- namespace
	}()
	select {
	case namespace := <-created:
		t.Cleanup(func() {
			_ = namespace.Close()
			if name != "" {
				if err := netns.DeleteNamed(name); err != nil {
					t.Errorf("delete network namespace %s: %v", name, err)
				}
			}
		})
		return namespace
	case err := <-failed:
		if mountpoint != "" {
			if _, statErr := os.Lstat(mountpoint); statErr == nil {
				if removeErr := os.Remove(mountpoint); removeErr != nil {
					t.Errorf("remove unmounted network namespace file %s: %v", mountpoint, removeErr)
				}
			}
		}
		t.Fatalf("create network namespace %q: %v", name, err)
		return netns.None()
	}
}

func namespaceHandle(t *testing.T, namespace netns.NsHandle) *netlink.Handle {
	t.Helper()
	handle, err := netlink.NewHandleAt(namespace)
	if err != nil {
		t.Fatalf("open netlink handle: %v", err)
	}
	t.Cleanup(func() {
		_ = handle.Close()
	})
	return handle
}

func linkByName(t *testing.T, handle *netlink.Handle, name string) netlink.Link {
	t.Helper()
	link, err := handle.LinkByName(name)
	if err != nil {
		t.Fatalf("look up %s: %v", name, err)
	}
	return link
}

func setLinkUp(t *testing.T, handle *netlink.Handle, name string) {
	t.Helper()
	if err := handle.LinkSetUp(linkByName(t, handle, name)); err != nil {
		t.Fatalf("set %s up: %v", name, err)
	}
}

func setLinkDown(t *testing.T, handle *netlink.Handle, name string) {
	t.Helper()
	if err := handle.LinkSetDown(linkByName(t, handle, name)); err != nil {
		t.Fatalf("set %s down: %v", name, err)
	}
}

func interfaceAddress(t *testing.T, cidr string) *netlink.Addr {
	t.Helper()
	address, err := netlink.ParseAddr(cidr)
	if err != nil {
		t.Fatalf("parse %s: %v", cidr, err)
	}
	address.Flags = unix.IFA_F_NODAD
	return address
}

func addAddress(t *testing.T, handle *netlink.Handle, name string, cidr string) {
	t.Helper()
	if err := handle.AddrAdd(linkByName(t, handle, name), interfaceAddress(t, cidr)); err != nil {
		t.Fatalf("add %s to %s: %v", cidr, name, err)
	}
}

func deleteAddress(t *testing.T, handle *netlink.Handle, name string, cidr string) {
	t.Helper()
	if err := handle.AddrDel(linkByName(t, handle, name), interfaceAddress(t, cidr)); err != nil {
		t.Fatalf("delete %s from %s: %v", cidr, name, err)
	}
}

// waitFor polls condition until it returns true or timeout passes.
func waitFor(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, description)
		}
		time.Sleep(pollStep)
	}
}

// commandProcess is one running configs-network-watch process and the lines
// it writes to stderr.
type commandProcess struct {
	command *exec.Cmd
	lines   chan string
	mutex   sync.Mutex
	output  strings.Builder
	done    chan struct{}
}

func startCommand(t *testing.T, namespace netns.NsHandle, binary string, args ...string) *commandProcess {
	t.Helper()
	command := exec.Command(binary, args...)
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatalf("open stderr pipe: %v", err)
	}
	process := &commandProcess{
		command: command,
		lines:   make(chan string, 1024),
		done:    make(chan struct{}),
	}
	start := command.Start
	if namespace.IsOpen() {
		start = func() error {
			return inNamespace(namespace, command.Start)
		}
	}
	if err := start(); err != nil {
		t.Fatalf("start %s: %v", strings.Join(args, " "), err)
	}
	go func() {
		defer close(process.done)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			process.mutex.Lock()
			process.output.WriteString(line + "\n")
			process.mutex.Unlock()
			select {
			case process.lines <- line:
			default:
			}
		}
	}()
	t.Cleanup(func() {
		if process.command.ProcessState == nil {
			_ = process.command.Process.Kill()
			<-process.done
			_ = process.command.Wait()
		}
		if t.Failed() {
			t.Logf("configs-network-watch output:\n%s", process.capturedOutput())
		}
	})
	return process
}

func (process *commandProcess) capturedOutput() string {
	process.mutex.Lock()
	defer process.mutex.Unlock()
	return process.output.String()
}

// waitForLine reads stderr lines until one contains text and returns it.
func (process *commandProcess) waitForLine(t *testing.T, timeout time.Duration, text string) string {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case line := <-process.lines:
			if strings.Contains(line, text) {
				return line
			}
		case <-timer.C:
			t.Fatalf("timed out after %s waiting for a line with %q", timeout, text)
			return ""
		}
	}
}

// linesFor reads the stderr lines that arrive during duration.
func (process *commandProcess) linesFor(duration time.Duration) []string {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	lines := make([]string, 0)
	for {
		select {
		case line := <-process.lines:
			lines = append(lines, line)
		case <-timer.C:
			return lines
		}
	}
}

// limitNewDescriptors lowers the process open file limit and returns a
// function that restores it.
func (process *commandProcess) limitNewDescriptors(t *testing.T) func() {
	t.Helper()
	pid := process.command.Process.Pid
	var original unix.Rlimit
	if err := unix.Prlimit(pid, unix.RLIMIT_NOFILE, nil, &original); err != nil {
		t.Fatalf("read the open file limit of %d: %v", pid, err)
	}
	limited := unix.Rlimit{Cur: standardDescriptorCount, Max: original.Max}
	if err := unix.Prlimit(pid, unix.RLIMIT_NOFILE, &limited, nil); err != nil {
		t.Fatalf("lower the open file limit of %d: %v", pid, err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if err := unix.Prlimit(pid, unix.RLIMIT_NOFILE, &original, nil); err != nil {
			t.Errorf("restore the open file limit of %d: %v", pid, err)
		}
	}
	t.Cleanup(restore)
	return restore
}

// stop sends stopSignal and returns the exit status.
func (process *commandProcess) stop(t *testing.T, stopSignal syscall.Signal) int {
	t.Helper()
	if err := process.command.Process.Signal(stopSignal); err != nil {
		t.Fatalf("send %s: %v", stopSignal, err)
	}
	return process.wait(t)
}

// wait waits for the process to exit and returns the exit status.
func (process *commandProcess) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-process.done:
	case <-time.After(processStopWait):
		t.Fatalf("process did not exit within %s", processStopWait)
	}
	err := process.command.Wait()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("wait for process: %v", err)
	}
	return process.command.ProcessState.ExitCode()
}
