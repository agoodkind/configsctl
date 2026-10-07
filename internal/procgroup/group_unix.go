//go:build unix

package procgroup

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const groupPollInterval = 50 * time.Millisecond

// RunGroup requires a command created by [exec.CommandContext].
func RunGroup(cmd *exec.Cmd) error {
	terminal := foregroundTerminal()
	if terminal >= 0 {
		defer closeTerminal(terminal)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Foreground: true, Ctty: terminal}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	cmd.Cancel = func() error {
		return signalProcess(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = StopGrace
	if err := cmd.Start(); err != nil {
		slog.Error("procgroup.start_failed", "command", cmd.Path, "err", err)
		return fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	if terminal >= 0 {
		signal.Ignore(syscall.SIGTTOU)
	}
	err := cmd.Wait()
	if terminal >= 0 {
		reclaimTerminal(terminal)
		signal.Reset(syscall.SIGTTOU)
	}
	stopGroup(cmd.Process.Pid)
	if err != nil {
		slog.Error("procgroup.run_failed", "command", cmd.Path, "err", err)
		return fmt.Errorf("run %s: %w", cmd.Path, err)
	}
	return nil
}

func foregroundTerminal() int {
	terminal, err := syscall.Open("/dev/tty", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1
	}
	foreground, err := unix.IoctlGetInt(terminal, unix.TIOCGPGRP)
	if err != nil || foreground != syscall.Getpgrp() {
		closeTerminal(terminal)
		return -1
	}
	return terminal
}

func reclaimTerminal(terminal int) {
	if err := unix.IoctlSetPointerInt(terminal, unix.TIOCSPGRP, syscall.Getpgrp()); err != nil {
		slog.Error("procgroup.terminal_reclaim_failed", "err", err)
	}
}

func closeTerminal(terminal int) {
	if err := syscall.Close(terminal); err != nil {
		slog.Error("procgroup.terminal_close_failed", "err", err)
	}
}

func stopGroup(groupID int) {
	if err := signalProcess(-groupID, syscall.SIGTERM); err != nil {
		return
	}
	grace := time.NewTimer(StopGrace)
	defer grace.Stop()
	poll := time.NewTicker(groupPollInterval)
	defer poll.Stop()
	for !errors.Is(syscall.Kill(-groupID, 0), syscall.ESRCH) {
		select {
		case <-grace.C:
			killGroup(groupID)
			return
		case <-poll.C:
		}
	}
}

func killGroup(groupID int) {
	slog.Info("procgroup.group_killed", "group", groupID)
	if err := signalProcess(-groupID, syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
		slog.Error("procgroup.group_kill_failed", "group", groupID, "err", err)
	}
}
