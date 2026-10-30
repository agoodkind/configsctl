// Package tofurun runs one OpenTofu command for a deployment controller.
// Run builds the child environment from an explicit allowlist, redacts
// declared secrets from progress output, and runs OpenTofu as a process
// group. Run starts a mutating command only with a valid hostlock.Scope.
package tofurun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"

	"goodkind.io/configsctl/hostlock"
	localrun "goodkind.io/configsctl/internal/tofurun"
)

// OutputMode selects the destination of OpenTofu output.
type OutputMode string

// RedactedProgress writes redacted standard output and standard error to the
// process standard output and to a private run log. PrivateJSON writes the
// unchanged standard output of "show -json <saved-plan>" to JSONPath and
// writes redacted standard error to the process standard error and to a
// private run log.
const (
	RedactedProgress OutputMode = "redacted_progress"
	PrivateJSON      OutputMode = "private_json"
)

// Request specifies one OpenTofu command.
//
// Binary is the absolute path of the OpenTofu executable. Root is the
// absolute path of the configuration root. Workspace is the existing
// OpenTofu CLI workspace that plan, apply, and show without a saved plan require.
//
// Args lists the OpenTofu arguments, starting with the subcommand. Run
// accepts init, workspace select, plan, show, validate, fmt -check, and apply
// of a saved plan. Value flags use the -name=value form.
//
// Environment is the source environment, such as [os.Environ]. Run reads only
// PATH, HOME, TMPDIR, LANG, TF_IN_AUTOMATION, and the names in
// RequiredEnvironment, SecretEnvironment, and SecretFiles from it. Run never
// adds the parent environment. Run registers SecretEnvironment values and the
// contents of SecretFiles with the redactor. SecretFiles maps an environment
// variable name to the absolute path of a private file, and Environment must
// assign that exact path to the name.
//
// OutputMode selects the output destination. JSONPath is the new private file
// for PrivateJSON. RequiredHosts lists the hosts that a mutation requires in
// its scope.
type Request struct {
	Binary, Root, Workspace                                   string
	Args, Environment, RequiredEnvironment, SecretEnvironment []string
	SecretFiles                                               map[string]string
	OutputMode                                                OutputMode
	JSONPath                                                  string
	RequiredHosts                                             []string
}

// Result reports the OpenTofu exit status and the private run log path.
// ExitCode is 0 for success, 1 for an error, 2 for a plan with changes under
// -detailed-exitcode, and 11 for an OpenTofu crash.
type Result struct {
	ExitCode int
	LogPath  string
}

// ErrIncomplete reports that OpenTofu did not start, ended on a signal, or
// still ran when the context ended.
var ErrIncomplete = localrun.ErrIncomplete

// ErrRejected reports a request that Run refused before starting OpenTofu.
var ErrRejected = errors.New("tofurun refused the request")

// ErrProviderLockChanged reports an init that changed the provider lock file.
var ErrProviderLockChanged = errors.New("OpenTofu init changed the provider lock file")

// Run validates request, checks scope for a mutation, and runs OpenTofu in
// request.Root. Run returns an error wrapping ErrRejected or
// hostlock.ErrScope before starting OpenTofu. Run returns an OpenTofu exit
// status in Result.ExitCode with a nil error. Canceling ctx sends SIGTERM to
// the OpenTofu process group and SIGKILL after the process group grace
// period, and Run returns an error wrapping ErrIncomplete.
func Run(ctx context.Context, request Request, scope *hostlock.Scope) (Result, error) {
	prepared, err := prepare(request, scope)
	if err != nil {
		return Result{ExitCode: 0, LogPath: ""}, err
	}
	defer prepared.cleanup()
	return prepared.execute(ctx)
}

// prepared is a validated request with its child arguments and environment.
type prepared struct {
	request     Request
	command     command
	binary      string
	args        []string
	environment environment
	removals    []string
	scope       *hostlock.Scope
}

func prepare(request Request, scope *hostlock.Scope) (*prepared, error) {
	binary, args, err := printable(request.Binary, request.Args)
	if err != nil {
		return nil, err
	}
	parsed, err := parseCommand(args)
	if err != nil {
		return nil, err
	}
	if err := parsed.check(request, scope != nil); err != nil {
		return nil, err
	}
	if err := checkScope(parsed, scope, request.RequiredHosts); err != nil {
		return nil, err
	}
	if err := checkLocations(request); err != nil {
		return nil, err
	}
	built, err := buildEnvironment(request)
	if err != nil {
		return nil, err
	}
	if err := checkSecretArguments(request.Args, built.patterns); err != nil {
		return nil, err
	}
	if err := checkFiles(request, parsed, built); err != nil {
		return nil, err
	}
	result := &prepared{
		request: request, command: parsed, binary: binary, args: args,
		environment: built, removals: nil, scope: scope,
	}
	if err := result.copySavedPlan(); err != nil {
		result.cleanup()
		return nil, err
	}
	return result, nil
}

// checkScope checks scope for a mutation. prepare and execute both run this
// check, and execute runs it immediately before starting OpenTofu.
func checkScope(parsed command, scope *hostlock.Scope, requiredHosts []string) error {
	if !parsed.mutates() {
		return nil
	}
	if err := scope.Check(requiredHosts); err != nil {
		slog.Error("tofurun.mutation_refused", "subcommand", parsed.name, "err", err)
		return fmt.Errorf("start OpenTofu %s: %w", parsed.name, err)
	}
	return nil
}

// checkFiles checks JSONPath, the plan -out path, and the selected workspace.
func checkFiles(request Request, parsed command, built environment) error {
	if err := checkOutput(request, parsed, built); err != nil {
		return err
	}
	if out := parsed.savedPlanOutput(); out != "" {
		if _, err := privateOutputPath("the plan -out path", built.tmpdir, out); err != nil {
			return err
		}
	}
	if parsed.usesWorkspace() {
		return checkSelectedWorkspace(request.Root, request.Workspace)
	}
	return nil
}

// cleanup removes the private saved plan copies.
func (p *prepared) cleanup() {
	for _, path := range p.removals {
		if err := os.RemoveAll(path); err != nil {
			slog.Warn("tofurun.cleanup_failed", "path", path, "err", err)
		}
	}
}

func (p *prepared) execute(ctx context.Context) (Result, error) {
	lockBefore, err := p.providerLockDigest()
	if err != nil {
		return Result{ExitCode: 0, LogPath: ""}, err
	}
	sink, err := openOutput(p.request, p.command, p.environment)
	if err != nil {
		return Result{ExitCode: 0, LogPath: ""}, err
	}
	if err := checkScope(p.command, p.scope, p.request.RequiredHosts); err != nil {
		return Result{ExitCode: 0, LogPath: sink.logPath}, errors.Join(err, sink.close(false))
	}
	binary, args := p.binary, p.args
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = p.request.Root
	cmd.Env = p.environment.values
	cmd.Stdin = nil
	cmd.Stdout = sink.stdout
	cmd.Stderr = sink.stderr
	slog.Info("tofurun.command_started", "root", p.request.Root, "workspace", p.request.Workspace,
		"subcommand", p.command.name, "mutation", p.command.mutates())
	exitCode, runErr := localrun.Execute(ctx, cmd)
	closeErr := sink.close(runErr == nil && exitCode == 0)
	result := Result{ExitCode: exitCode, LogPath: sink.logPath}
	if runErr == nil && exitCode < 0 {
		runErr = fmt.Errorf("%w: OpenTofu %s ended on a signal", ErrIncomplete, p.command.name)
		slog.Error("tofurun.command_incomplete", "root", p.request.Root, "subcommand", p.command.name, "err", runErr)
	}
	if runErr != nil {
		return result, errors.Join(runErr, closeErr)
	}
	if closeErr != nil {
		return result, closeErr
	}
	if exitCode == 0 {
		return result, p.checkProviderLock(lockBefore)
	}
	return result, nil
}
