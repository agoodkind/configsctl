//go:build unix

package procgroup

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
)

// Run requires a command created by [exec.CommandContext].
// Without a controlling terminal, cancellation sends SIGINT to a new child
// process group. os/exec kills the child after StopGrace if it has not exited.
// Run sends SIGKILL to the group after the child returns.
//
// With a controlling terminal, the child inherits the current process group
// so interactive commands can read the terminal. Cancellation signals only
// the child; descendants may continue running.
func Run(cmd *exec.Cmd) error {
	var canceled atomic.Bool
	group := !hasControllingTerminal()
	if group {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	cmd.Cancel = func() error {
		canceled.Store(true)
		if group {
			return signalProcess(-cmd.Process.Pid, syscall.SIGINT)
		}
		return signalProcess(cmd.Process.Pid, syscall.SIGINT)
	}
	cmd.WaitDelay = StopGrace
	err := cmd.Run()
	if group && canceled.Load() {
		if killErr := signalProcess(-cmd.Process.Pid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			slog.Error("procgroup.kill_failed", "pid", cmd.Process.Pid, "err", killErr)
		}
	}
	if err != nil {
		slog.Error("procgroup.run_failed", "command", cmd.Path, "err", err)
		return fmt.Errorf("run %s: %w", cmd.Path, err)
	}
	return nil
}

// signalProcess maps a missing process or group to [os.ErrProcessDone].
func signalProcess(pid int, sig syscall.Signal) error {
	err := syscall.Kill(pid, sig)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	slog.Error("procgroup.signal_failed", "pid", pid, "signal", sig.String(), "err", err)
	return fmt.Errorf("send %s to %d: %w", sig, pid, err)
}

func hasControllingTerminal() bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	_ = tty.Close()
	return true
}
