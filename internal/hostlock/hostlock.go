// Package hostlock takes, renews, and releases the deploy lock file on a host.
// Each operation runs the embedded lock script on the host with bash.
package hostlock

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed lock.sh
var lockScript []byte

// DefaultDir is the lock directory on every host.
const DefaultDir = "/var/lib/configs"

// TTL is the time after the last renewal when a lock counts as free.
const TTL = 5 * time.Minute

// RenewInterval is the time between two renewals of a held lock.
const RenewInterval = time.Minute

const killedCommandWait = time.Second

// Operation is one action of the lock script.
type Operation string

// The lock script actions.
const (
	Acquire Operation = "acquire"
	Renew   Operation = "renew"
	Release Operation = "release"
	Unlock  Operation = "unlock"
)

// Exit statuses of the lock script.
const (
	exitHeld    = 3
	exitNotHeld = 4
)

// Host is one lock target. Do runs the script over ssh as User at Address.
// An empty Address runs the script on this machine.
type Host struct {
	Name    string
	User    string
	Address string
	Dir     string
}

// HeldError reports a current lock of another run.
type HeldError struct {
	Host       string
	Run        string
	Controller string
	Expiry     time.Time
}

// Error states the host and the run, controller, and expiry of the lock.
func (e *HeldError) Error() string {
	return fmt.Sprintf("host %s is locked by run %s on %s until %s",
		e.Host, e.Run, e.Controller, e.Expiry.UTC().Format(time.RFC3339))
}

// ErrNotHeld reports that the lock file on a host names another run.
var ErrNotHeld = errors.New("the lock on the host belongs to another run")

// Do runs one operation on host for run, owned by controller.
func Do(ctx context.Context, host Host, op Operation, run, controller string) error {
	dir := host.Dir
	if dir == "" {
		dir = DefaultDir
	}
	script := []string{"-s", "--", string(op), run, controller, strconv.Itoa(int(TTL / time.Second)), dir}
	var cmd *exec.Cmd
	if host.Address == "" {
		args, err := checkedArgs(script)
		if err != nil {
			return err
		}
		cmd = exec.CommandContext(ctx, "bash", args...)
	} else {
		remote := append([]string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host.User + "@" + host.Address, "bash"}, script...)
		args, err := checkedArgs(remote)
		if err != nil {
			return err
		}
		cmd = exec.CommandContext(ctx, "ssh", args...)
	}
	cmd.Stdin = bytes.NewReader(lockScript)
	cmd.WaitDelay = killedCommandWait
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.ExitCode() {
		case exitHeld:
			return heldError(host.Name, stdout.String())
		case exitNotHeld:
			return fmt.Errorf("%s on %s: %w (%s)", op, host.Name, ErrNotHeld, strings.TrimSpace(stdout.String()))
		}
	}
	slog.Error("hostlock.operation_failed", "host", host.Name, "operation", string(op),
		"stderr", strings.TrimSpace(stderr.String()), "err", err)
	return fmt.Errorf("%s lock on %s: %w: %s", op, host.Name, err, strings.TrimSpace(stderr.String()))
}

func heldError(host, line string) error {
	fields := strings.Fields(line)
	held := &HeldError{Host: host, Run: "", Controller: "", Expiry: time.Time{}}
	if len(fields) == 3 {
		held.Run = fields[0]
		held.Controller = fields[1]
		if seconds, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
			held.Expiry = time.Unix(seconds, 0)
		}
	}
	return held
}

// printableArg accepts printable ASCII only. Do passes every argument to a
// child process as one argv entry.
var printableArg = regexp.MustCompile(`^[\x20-\x7E]+$`)

func checkedArgs(args []string) ([]string, error) {
	safe := make([]string, 0, len(args))
	for _, arg := range args {
		match := printableArg.FindString(arg)
		if match != arg {
			return nil, fmt.Errorf("lock argument %q contains a non-printable character", arg)
		}
		safe = append(safe, match)
	}
	return safe, nil
}
