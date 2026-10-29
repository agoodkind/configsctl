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
	cmd := command(ctx, request.Root, display, request.Args)
	cmd.Dir = request.Root
	cmd.Env = environment
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	var log *runlog.Log
	if tofuargs.Streams(display) {
		log, err = runlog.Open("tofu-"+string(tofuargs.Subcommand(display)), activePatterns())
		if err != nil {
			slog.Error("tofu.log_open_failed", "workspace", request.Root, "err", err)
			return Result{ExitCode: 0, LogPath: ""}, fmt.Errorf("open the OpenTofu run log: %w", err)
		}
		cmd.Stdout = log
		cmd.Stderr = log
		log.Announce("Tofu")
	}

	runErr := procgroup.RunGroup(cmd)
	result := Result{ExitCode: 0, LogPath: ""}
	var closeErr error
	if log != nil {
		result.LogPath = log.Path()
		if err := log.Close(); err != nil {
			slog.Error("tofu.log_close_failed", "log", result.LogPath, "err", err)
			closeErr = fmt.Errorf("close the OpenTofu run log: %w", err)
		}
	}
	if runErr == nil {
		return result, closeErr
	}
	var exited *exec.ExitError
	if context.Cause(ctx) == nil && errors.As(runErr, &exited) {
		result.ExitCode = exited.ExitCode()
		return result, closeErr
	}
	slog.Error("tofu.command_incomplete", "workspace", request.Root, "err", runErr)
	return result, fmt.Errorf("%w: %w", ErrIncomplete, runErr)
}

// command logs the root and subcommand from display at command start.
// It builds an [exec.Cmd] for tofu with args.
func command(ctx context.Context, root string, display, args []string) *exec.Cmd {
	slog.Info("tofu.command_started", "workspace", root, "subcommand", tofuargs.Subcommand(display))
	return exec.CommandContext(ctx, "tofu", args...)
}
