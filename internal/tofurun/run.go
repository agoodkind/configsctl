// Package tofurun runs one OpenTofu command for configsctl tofu.
// It starts tofu from PATH as a process group and reports its exit status.
// It writes redacted output to the terminal or a private run log.
// Only code in the configsctl module can import this package.
package tofurun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"

	"goodkind.io/configsctl/internal/procgroup"
	"goodkind.io/configsctl/internal/redact"
	"goodkind.io/configsctl/internal/runlog"
	"goodkind.io/configsctl/internal/tofuargs"
)

// ErrIncomplete reports that OpenTofu did not start or that the context ended
// before OpenTofu exited. An ordinary nonzero exit status does not produce
// this error.
var ErrIncomplete = errors.New("the OpenTofu process did not finish")

// Request specifies an OpenTofu command. Root is the root module directory.
// Args supplies tofu arguments. Environment supplies the complete child
// environment. Run does not add the parent environment.
type Request struct {
	Root        string
	Args        []string
	Environment []string
	// DisplayArgs contains the arguments the operator typed. Run uses them to
	// select the terminal or a run log and the log filename. An empty value
	// selects Args. The local command sets DisplayArgs for a private plan or
	// saved plan that replaces the operator's apply.
	DisplayArgs []string
	// Patterns supplies the caller's secret values for run log redaction.
	Patterns []redact.Pattern
}

// Result reports the OpenTofu exit status in ExitCode and the run log path in
// LogPath. ExitCode can report 0, 1, 2 for -detailed-exitcode, or 11 for a
// crash. LogPath is empty when Run writes output to the terminal.
type Result struct {
	ExitCode int
	LogPath  string
}

// Run starts tofu in Root as a process group and connects standard input.
// An ordinary nonzero exit returns its ExitCode and a nil error.
// Run returns an error for a nil Environment, a run log failure, or a
// wrapped ErrIncomplete.
func Run(ctx context.Context, request Request) (Result, error) {
	environment, err := childEnvironment(request)
	if err != nil {
		return Result{ExitCode: 0, LogPath: ""}, err
	}
	display := request.DisplayArgs
	if len(display) == 0 {
		display = request.Args
	}
	slog.Info("tofu.command_started", "workspace", request.Root, "subcommand", tofuargs.Subcommand(display))
	args := request.Args
	cmd := exec.CommandContext(ctx, "tofu", args...)
	cmd.Dir = request.Root
	cmd.Env = environment
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	var log *runlog.Log
	if tofuargs.Streams(display) {
		log, err = openOutput(request, display)
		if err != nil {
			return Result{ExitCode: 0, LogPath: ""}, err
		}
		cmd.Stdout = log
		cmd.Stderr = log
	}

	exitCode, runErr := Execute(ctx, cmd)
	result := Result{ExitCode: exitCode, LogPath: ""}
	var closeErr error
	if log != nil {
		result.LogPath = log.Path()
		closeErr = closeOutput(log)
	}
	if runErr == nil {
		return result, closeErr
	}
	if closeErr == nil {
		return result, runErr
	}
	slog.Error("tofu.command_incomplete", "workspace", request.Root, "err", closeErr)
	return result, errors.Join(runErr, closeErr)
}

// Execute runs cmd with procgroup.RunGroup. cmd must come from
// [exec.CommandContext] with ctx. Execute returns the exit status and a nil
// error when the process exits before ctx ends. Otherwise it returns an error
// wrapping ErrIncomplete.
func Execute(ctx context.Context, cmd *exec.Cmd) (int, error) {
	runErr := procgroup.RunGroup(cmd)
	if runErr == nil {
		return 0, nil
	}
	var exited *exec.ExitError
	if context.Cause(ctx) == nil && errors.As(runErr, &exited) {
		return exited.ExitCode(), nil
	}
	slog.Error("tofu.command_incomplete", "workspace", cmd.Dir, "err", runErr)
	return 0, fmt.Errorf("%w: %w", ErrIncomplete, runErr)
}
