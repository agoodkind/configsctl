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

// Operation is one action of the lock script.
type Operation string

// The lock script actions.
const (
	Acquire Operation = "acquire"
	Renew   Operation = "renew"
	Release Operation = "release"
	Unlock  Operation = "unlock"
)

// MaxExtend caps declared leases at 900 seconds.
const MaxExtend = 15 * time.Minute

const extend Operation = "extend"

// Exit statuses of the lock script.
const (
	exitHeld          = 3
	exitNotHeld       = 4
	exitExtendRefused = 6
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

// ErrExtendRefused reports a lease shorter than one second or longer than
// MaxExtend.
var ErrExtendRefused = errors.New("the declared lease is outside the allowed range")

// Do runs one operation on host for run, owned by controller.
func Do(ctx context.Context, host Host, op Operation, run, controller string) error {
	_, err := runScript(ctx, host, command{op: op, run: run, controller: controller, lease: TTL})
	return err
}

type command struct {
	op         Operation
	run        string
	controller string
	lease      time.Duration
}

// answer is the lease that the lock script wrote on the host.
type answer struct {
	expiry    time.Time
	remaining time.Duration
}

func noLease() answer {
	return answer{expiry: time.Time{}, remaining: 0}
}

func runScript(ctx context.Context, host Host, request command) (answer, error) {
	dir := host.Dir
	if dir == "" {
		dir = DefaultDir
	}
	script := []string{
		"-s", "--", string(request.op), request.run, request.controller,
		strconv.FormatInt(int64(request.lease/time.Second), 10), dir,
		strconv.FormatInt(int64(MaxExtend/time.Second), 10),
	}
	var cmd *exec.Cmd
	if host.Address == "" {
		args, err := checkedArgs(script)
		if err != nil {
			return noLease(), err
		}
		cmd = exec.CommandContext(ctx, "bash", args...)
	} else {
		remote := append([]string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host.User + "@" + host.Address, "bash"}, script...)
		args, err := checkedArgs(remote)
		if err != nil {
			return noLease(), err
		}
		cmd = exec.CommandContext(ctx, "ssh", args...)
	}
	cmd.Stdin = bytes.NewReader(lockScript)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return leaseAnswer(stdout.String()), nil
	}
	if refusal := scriptRefusal(host.Name, request.op, err, stdout.String(), stderr.String()); refusal != nil {
		return noLease(), refusal
	}
	slog.Error("hostlock.operation_failed", "host", host.Name, "operation", string(request.op),
		"stderr", strings.TrimSpace(stderr.String()), "err", err)
	return noLease(), fmt.Errorf("%s lock on %s: %w: %s", request.op, host.Name, err, strings.TrimSpace(stderr.String()))
}

// scriptRefusal returns the error for an exit status of the lock script that
// reports a refusal, and nil for any other failure.
func scriptRefusal(host string, op Operation, err error, stdout, stderr string) error {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return nil
	}
	switch exitErr.ExitCode() {
	case exitHeld:
		return heldError(host, stdout)
	case exitNotHeld:
		return fmt.Errorf("%s on %s: %w (%s)", op, host, ErrNotHeld, strings.TrimSpace(stdout))
	case exitExtendRefused:
		slog.Warn("hostlock.extend_refused", "host", host, "stderr", strings.TrimSpace(stderr), "err", err)
		return fmt.Errorf("%s on %s: %w: %s", op, host, ErrExtendRefused, strings.TrimSpace(stderr))
	default:
		return nil
	}
}

// leaseAnswer reads "<expiry> <host time>" from the script output. release
// and unlock print no lease.
func leaseAnswer(line string) answer {
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return noLease()
	}
	expiry, expiryErr := strconv.ParseInt(fields[0], 10, 64)
	hostTime, hostTimeErr := strconv.ParseInt(fields[1], 10, 64)
	if expiryErr != nil || hostTimeErr != nil {
		return noLease()
	}
	return answer{expiry: time.Unix(expiry, 0), remaining: time.Duration(expiry-hostTime) * time.Second}
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
