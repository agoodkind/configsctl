package main_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHostLockServerRejectsCallerDirectoryBeforeVaultSetup(t *testing.T) {
	command := exec.CommandContext(
		t.Context(),
		configsctlBinary,
		"host-lock",
		"serve",
		"--principal",
		"planning",
	)
	command.Dir = t.TempDir()
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	command.Stdin = strings.NewReader(
		`{"protocol_version":1,"operation":"acquire","run_id":"attempt","lease_seconds":30,"directory":"/tmp"}`,
	)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "supported JSON fields") {
		t.Fatalf(
			"host-lock serve did not reject the caller directory before vault setup: %v: %s",
			err,
			output,
		)
	}
}

func TestHostLockServerStopsDuringIncompleteRequest(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	if _, err := writer.WriteString("{"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		configsctlBinary,
		"host-lock",
		"serve",
		"--principal",
		"planning",
	)
	command.Stdin = input
	command.Dir = t.TempDir()
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	// Wait until the endpoint reads the first byte before sending the signal.
	fds := []unix.PollFd{{Fd: int32(input.Fd()), Events: unix.POLLIN}}

	for {
		ready, err := unix.Poll(fds, 0)
		if err != nil || ctx.Err() != nil {
			t.Fatalf("wait for the endpoint to read stdin: %v, %v", err, ctx.Err())
		}

		if ready == 0 {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	if err := command.Wait(); err == nil || ctx.Err() != nil ||
		!strings.Contains(stderr.String(), "cannot be read") {
		t.Fatalf(
			"SIGTERM did not interrupt the incomplete request: %v, %v, %s",
			err,
			ctx.Err(),
			stderr.String(),
		)
	}
}
