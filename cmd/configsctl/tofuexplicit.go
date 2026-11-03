package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"goodkind.io/configsctl/hostlock"
	"goodkind.io/configsctl/internal/tofuargs"
	"goodkind.io/configsctl/internal/tofulock"
	"goodkind.io/configsctl/tofurun"
)

const (
	explicitDirectoryPattern = "configsctl-explicit-*"
	explicitDirectoryMode    = 0o700
	explicitPlanJSONName     = "mutation-plan.json"
	// explicitDefaultParallelism is the OpenTofu default. tofurun requires the flag.
	explicitDefaultParallelism = 10
	providerConfigVariable     = "TF_CLI_CONFIG_FILE"
	selectedWorkspaceFile      = ".terraform/environment"
	defaultTofuWorkspace       = "default"
)

type explicitOption string

const (
	optionAutoApprove explicitOption = "auto-approve"
	optionNoColor     explicitOption = "no-color"
	optionInput       explicitOption = "input"
	optionParallelism explicitOption = "parallelism"
	optionLockTimeout explicitOption = "lock-timeout"
)

type explicitOptions struct {
	parallelism int
	lockTimeout string
}

func explicitLockTargets(request tofuLockRequest) (tofulock.Targets, bool) {
	var none tofulock.Targets
	if len(request.args) == 0 || request.args[0] != string(tofuargs.Apply) {
		return none, false
	}
	targets, declared := request.lockTargets[tofuWorkspaceKey(request.workspaceDir)]
	if !declared || len(targets.LockEndpoints) == 0 {
		return none, false
	}
	return targets, true
}

// requireExplicitLockPath refuses an apply or destroy in a workspace that
// declares lock_endpoints when the explicit path cannot run the command. The
// inventory lock path reads the Ansible inventory for these commands. This
// check stops the command before it starts that path.
func requireExplicitLockPath(tofu tofuSettings, workspaceDir string, args []string) error {
	command := tofuargs.Subcommand(args)
	if command != tofuargs.Apply && command != tofuargs.Destroy {
		return nil
	}
	if tofuargs.BooleanArgument(args, "help", false) || tofuargs.BooleanArgument(args, "h", false) {
		return nil
	}
	workspace := tofuWorkspaceKey(workspaceDir)
	targets := tofu.LockTargets[workspace]
	if len(targets.LockEndpoints) == 0 {
		return nil
	}
	reason := explicitRefusalReason(tofu, args, command)
	if reason == "" {
		return nil
	}
	endpoints := slices.Sorted(maps.Keys(targets.LockEndpoints))
	err := fmt.Errorf("workspace %s declares lock_endpoints %v, but the explicit lock path cannot run %s: %s",
		workspace, endpoints, command, reason)
	slog.Error("tofu.mutation.explicit_path_unavailable", "workspace", workspace,
		"lock_endpoints", endpoints, "command", string(command), "reason", reason, "err", err)
	return err
}

func requiredCredentialHosts(credential tofuCredentialSettings, usesCredential bool, workspaceDir string, childEnv []string) ([]string, error) {
	if !usesCredential {
		return nil, nil
	}
	if err := credential.validateWorkspace(workspaceDir, childEnv); err != nil {
		return nil, err
	}
	return []string{credential.LockHost}, nil
}

// runEndpointHelp runs the OpenTofu help command for apply or destroy in a
// workspace that declares lock_endpoints. The help command takes no lock and
// reads no inventory. The first result reports whether it handled the command.
func runEndpointHelp(env cmdEnv, tofu tofuSettings, workspaceDir string, args, childEnv []string) (bool, error) {
	help := tofuargs.BooleanArgument(args, "help", false) || tofuargs.BooleanArgument(args, "h", false)
	if !help || !tofuMutates(args) {
		return false, nil
	}
	if len(tofu.LockTargets[tofuWorkspaceKey(workspaceDir)].LockEndpoints) == 0 {
		return false, nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return true, executeTofu(ctx, env, workspaceDir, args, args, childEnv)
}

// validateEndpointCredentials refuses a workspace that declares both
// lock_endpoints and a credential. The credential path locks through the
// inventory, which the explicit lock path does not read.
func validateEndpointCredentials(tofu tofuSettings) error {
	for _, workspace := range slices.Sorted(maps.Keys(tofu.LockTargets)) {
		if len(tofu.LockTargets[workspace].LockEndpoints) == 0 {
			continue
		}
		if _, credential := tofu.Credentials[workspace]; !credential {
			continue
		}
		err := fmt.Errorf("workspace %s declares both tofu.lock_targets lock_endpoints and tofu.credentials; the credential path locks through the inventory", workspace)
		slog.Error("tofu.settings.endpoints_with_credential", "workspace", workspace, "err", err)
		return err
	}
	return nil
}

func explicitRefusalReason(tofu tofuSettings, args []string, command tofuargs.Command) string {
	switch {
	case command == tofuargs.Destroy:
		return "the explicit lock path runs apply only, and destroy would use the inventory lock path"
	case tofu.GuestModules == nil:
		return "tofu.guest_modules is unset, and the command would use the inventory lock path"
	case args[0] != string(tofuargs.Apply):
		return "the command must start with apply, and any other order would use the inventory lock path"
	case savedPlanCandidate(args) != "":
		return "the command includes a saved plan, and the explicit lock path creates its own plan"
	}
	return ""
}

// runExplicitMutation plans, reads the plan, locks the declared endpoints, and
// applies the recorded plan. It does not read the Ansible inventory or run an
// Ansible program, and it gives OpenTofu an explicit environment.
func runExplicitMutation(ctx context.Context, settings tofuSettings, request tofuLockRequest, targets tofulock.Targets, passwordFile string) error {
	options, err := parseExplicitArguments(request.args, targets.MaxParallelism)
	if err != nil {
		return err
	}
	runner, cleanup, err := newExplicitRunner(settings, request, passwordFile)
	if err != nil {
		return err
	}
	defer cleanup()
	planArgs := explicitPlanArguments(options, runner.planPath)
	planResult, err := runner.run(ctx, "plan", planArgs, tofurun.RedactedProgress, "", nil, nil)
	if err != nil {
		return err
	}
	changed := planResult.ExitCode == tofuPendingExitCode
	if planResult.ExitCode != 0 && !changed {
		return tofuFailure(ctx, &exitCodeError{code: planResult.ExitCode}, planResult.LogPath)
	}
	plannedDigest, err := planDigest(runner.planPath)
	if err != nil {
		return err
	}
	plan, err := runner.showPlan(ctx)
	if err != nil {
		return err
	}
	decision := tofulock.DecideIndependent(plan, targets)
	if decision.AllHosts {
		return fmt.Errorf("workspace %s has unknown lock ownership: %s", request.workspaceDir, decision.Reason)
	}
	hosts, err := targets.LockHosts(decision)
	if err != nil {
		slog.Error("tofu.mutation.explicit_lock_hosts_failed", "workspace", request.workspaceDir, "err", err)
		return fmt.Errorf("select the lock hosts of workspace %s: %w", request.workspaceDir, err)
	}
	names := lockHostNames(hosts)
	if err := requirePolicyScope(request.workspaceDir, targets, names); err != nil {
		return err
	}
	if changed && !tofuargs.AutoApprove(request.args) {
		if err := confirmMutation(ctx, request.args, request.env); err != nil {
			return err
		}
	}
	slog.Info("tofu.mutation.explicit_lock_hosts", "workspace", request.workspaceDir, "hosts", names)
	lockedCtx, scope, release, err := acquireExplicitLocks(ctx, hosts)
	if err != nil {
		return err
	}
	defer release()
	if err := requirePlanDigest(runner.planPath, plannedDigest); err != nil {
		return err
	}
	applyArgs := explicitApplyArguments(options, runner.planPath)
	applyResult, err := runner.run(lockedCtx, "apply", applyArgs, tofurun.RedactedProgress, "", scope, names)
	if err != nil {
		return err
	}
	if applyResult.ExitCode != 0 {
		return tofuFailure(lockedCtx, &exitCodeError{code: applyResult.ExitCode}, applyResult.LogPath)
	}
	return nil
}

// parseExplicitArguments accepts the options of "apply" that the public
// runner can run, each in the -name=value form. It refuses every other
// argument before OpenTofu starts.
func parseExplicitArguments(args []string, maxParallelism *int) (explicitOptions, error) {
	options := explicitOptions{parallelism: explicitDefaultParallelism, lockTimeout: ""}
	if maxParallelism != nil {
		options.parallelism = min(options.parallelism, *maxParallelism)
	}
	for _, argument := range args[1:] {
		if !strings.HasPrefix(argument, "-") {
			return options, errors.New("the explicit lock path does not accept a positional apply argument")
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		switch explicitOption(name) {
		case optionAutoApprove, optionNoColor:
			if !hasValue {
				continue
			}
			if _, err := strconv.ParseBool(value); err != nil {
				slog.Error("tofu.mutation.explicit_option_invalid", "option", name, "err", err)
				return options, fmt.Errorf("invalid -%s value: %w", name, err)
			}
		case optionInput:
			if enabled, err := strconv.ParseBool(value); !hasValue || err != nil || enabled {
				return options, errors.New("the explicit lock path requires -input=false")
			}
		case optionParallelism:
			parallelism, err := strconv.Atoi(value)
			if !hasValue || err != nil || parallelism < 1 {
				return options, errors.New("-parallelism requires the -parallelism=N form with a positive integer N")
			}
			if maxParallelism != nil && parallelism > *maxParallelism {
				return options, fmt.Errorf("-parallelism=%d exceeds the workspace max_parallelism %d", parallelism, *maxParallelism)
			}
			options.parallelism = parallelism
		case optionLockTimeout:
			if !hasValue || value == "" {
				return options, errors.New("-lock-timeout requires the -lock-timeout=DURATION form")
			}
			options.lockTimeout = value
		default:
			return options, fmt.Errorf("the explicit lock path does not accept the OpenTofu option -%s", name)
		}
	}
	return options, nil
}

func explicitPlanArguments(options explicitOptions, planPath string) []string {
	args := []string{
		string(tofuargs.Plan), "-input=false", "-no-color", "-detailed-exitcode",
		"-parallelism=" + strconv.Itoa(options.parallelism),
	}
	if options.lockTimeout != "" {
		args = append(args, "-lock-timeout="+options.lockTimeout)
	}
	return append(args, "-out="+planPath)
}

func explicitApplyArguments(options explicitOptions, planPath string) []string {
	args := []string{
		string(tofuargs.Apply), "-input=false", "-no-color",
		"-parallelism=" + strconv.Itoa(options.parallelism),
	}
	if options.lockTimeout != "" {
		args = append(args, "-lock-timeout="+options.lockTimeout)
	}
	return append(args, planPath)
}

// requirePolicyScope checks that the lock names include every required host of
// the workspace before the controller acquires any lock.
func requirePolicyScope(workspaceDir string, targets tofulock.Targets, names []string) error {
	if len(targets.RequiredHosts) == 0 {
		return nil
	}
	if len(names) == 0 {
		err := fmt.Errorf("workspace %s requires locks on hosts %v, but the lock scope is empty", workspaceDir, targets.RequiredHosts)
		slog.Error("tofu.policy.scope_empty", "workspace", workspaceDir, "err", err)
		return err
	}
	for _, required := range targets.RequiredHosts {
		if !slices.Contains(names, required) {
			err := fmt.Errorf("workspace %s requires a lock on host %s, but the lock scope %v omits it", workspaceDir, required, names)
			slog.Error("tofu.policy.scope_incomplete", "workspace", workspaceDir, "host", required, "scope", names, "err", err)
			return err
		}
	}
	return nil
}

func lockHostNames(hosts []hostlock.Host) []string {
	names := make([]string, 0, len(hosts))
	for _, host := range hosts {
		names = append(names, host.Name)
	}
	return names
}

type explicitRunner struct {
	base     tofurun.Request
	planPath string
	jsonPath string
}

// newExplicitRunner creates the private directory and the request that every
// command of the mutation shares. The caller calls cleanup to remove the
// directory.
//
// The request passes OpenTofu only PATH, LANG when set, TF_IN_AUTOMATION, the
// private HOME and TMPDIR, the provider mirror configuration path, and the
// vault values that the legacy path exports to the workspace.
func newExplicitRunner(settings tofuSettings, request tofuLockRequest, passwordFile string) (explicitRunner, func(), error) {
	var runner explicitRunner
	assignments, err := tofuSecretEnv(settings, request.workspaceDir, defaultVaultFile, passwordFile)
	if err != nil {
		return runner, nil, err
	}
	binary, err := tofuBinary()
	if err != nil {
		return runner, nil, err
	}
	root, err := filepath.Abs(request.workspaceDir)
	if err != nil {
		slog.Error("tofu.mutation.explicit_root_failed", "workspace", request.workspaceDir, "err", err)
		return runner, nil, fmt.Errorf("resolve the workspace directory %s: %w", request.workspaceDir, err)
	}
	workspace, err := selectedTofuWorkspace(root)
	if err != nil {
		return runner, nil, err
	}
	if err := requireWorkspaceMatch(request.env, workspace); err != nil {
		return runner, nil, err
	}
	directory, err := os.MkdirTemp("", explicitDirectoryPattern)
	if err != nil {
		slog.Error("tofu.mutation.explicit_directory_failed", "err", err)
		return runner, nil, fmt.Errorf("create a private mutation directory: %w", err)
	}
	cleanup := func() { removePrivateDirectory(directory) }
	home := filepath.Join(directory, "home")
	temporary := filepath.Join(directory, "tmp")
	for _, path := range []string{directory, home, temporary} {
		if err := makePrivateDirectory(path); err != nil {
			cleanup()
			return runner, nil, err
		}
	}
	environment := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + temporary, "TF_IN_AUTOMATION=1",
	}
	if language := os.Getenv("LANG"); language != "" {
		environment = append(environment, "LANG="+language)
	}
	required, redacted := splitExplicitAssignments(assignments)
	runner = explicitRunner{
		base: tofurun.Request{
			Binary: binary, Root: root, Workspace: workspace,
			Args: nil, Environment: append(environment, assignments...),
			RequiredEnvironment: required, SecretEnvironment: redacted, SecretFiles: nil,
			OutputMode: tofurun.RedactedProgress, JSONPath: "", RequiredHosts: nil,
		},
		planPath: filepath.Join(temporary, mutationPlanName),
		jsonPath: filepath.Join(temporary, explicitPlanJSONName),
	}
	return runner, cleanup, nil
}

// splitExplicitAssignments separates the provider mirror configuration path,
// which is not a secret, from the vault values that tofurun must redact.
func splitExplicitAssignments(assignments []string) (required, redacted []string) {
	for _, assignment := range assignments {
		name, _, _ := strings.Cut(assignment, "=")
		if name == providerConfigVariable {
			required = append(required, name)
			continue
		}
		redacted = append(redacted, name)
	}
	return required, redacted
}

// requireWorkspaceMatch refuses a TF_WORKSPACE value that differs from the
// selected workspace. tofurun does not pass TF_WORKSPACE to OpenTofu, and
// without this check the apply would use the selected workspace silently.
func requireWorkspaceMatch(environment []string, selected string) error {
	requested := environmentValue(environment, "TF_WORKSPACE")
	if requested == "" || requested == selected {
		return nil
	}
	err := fmt.Errorf("TF_WORKSPACE is %q, but the explicit lock path applies in the selected workspace %q", requested, selected)
	slog.Error("tofu.mutation.explicit_workspace_mismatch", "requested", requested, "selected", selected, "err", err)
	return err
}

func removePrivateDirectory(path string) {
	if err := os.RemoveAll(path); err != nil {
		slog.Warn("tofu.mutation.explicit_directory_remove_failed", "path", path, "err", err)
	}
}

func makePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, explicitDirectoryMode); err != nil {
		slog.Error("tofu.mutation.explicit_directory_failed", "path", path, "err", err)
		return fmt.Errorf("create the private directory %s: %w", path, err)
	}
	if err := os.Chmod(path, explicitDirectoryMode); err != nil {
		slog.Error("tofu.mutation.explicit_directory_failed", "path", path, "err", err)
		return fmt.Errorf("restrict the private directory %s: %w", path, err)
	}
	return nil
}

// selectedTofuWorkspace returns the OpenTofu workspace that root has selected.
// OpenTofu uses the default workspace without the environment file.
func selectedTofuWorkspace(root string) (string, error) {
	body, err := os.ReadFile(filepath.Join(root, selectedWorkspaceFile))
	if errors.Is(err, fs.ErrNotExist) {
		return defaultTofuWorkspace, nil
	}
	if err != nil {
		slog.Error("tofu.mutation.explicit_workspace_failed", "root", root, "err", err)
		return "", fmt.Errorf("read the selected workspace of %s: %w", root, err)
	}
	return strings.TrimSpace(string(body)), nil
}

func (r explicitRunner) run(
	ctx context.Context, step string, args []string, mode tofurun.OutputMode, jsonPath string,
	scope *hostlock.Scope, requiredHosts []string,
) (tofurun.Result, error) {
	request := r.base
	request.Args = args
	request.OutputMode = mode
	request.JSONPath = jsonPath
	request.RequiredHosts = requiredHosts
	result, err := tofurun.Run(ctx, request, scope)
	if errors.Is(err, tofurun.ErrIncomplete) {
		return result, tofuFailure(ctx, err, result.LogPath)
	}
	if err != nil {
		slog.Error("tofu.mutation.explicit_run_failed", "step", step, "workspace", request.Root, "err", err)
		return result, fmt.Errorf("run OpenTofu %s in %s: %w", step, request.Root, err)
	}
	return result, nil
}

func (r explicitRunner) showPlan(ctx context.Context) ([]byte, error) {
	args := []string{"show", "-json", r.planPath}
	result, err := r.run(ctx, "show", args, tofurun.PrivateJSON, r.jsonPath, nil, nil)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, tofuFailure(ctx, &exitCodeError{code: result.ExitCode}, result.LogPath)
	}
	plan, err := os.ReadFile(r.jsonPath)
	if err != nil {
		slog.Error("tofu.mutation.explicit_plan_json_failed", "path", r.jsonPath, "err", err)
		return nil, fmt.Errorf("read the plan JSON %s: %w", r.jsonPath, err)
	}
	return plan, nil
}

func planDigest(path string) ([sha256.Size]byte, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		slog.Error("tofu.mutation.explicit_plan_unreadable", "plan", path, "err", err)
		return [sha256.Size]byte{}, fmt.Errorf("read the recorded plan %s: %w", path, err)
	}
	return sha256.Sum256(contents), nil
}

func requirePlanDigest(path string, recorded [sha256.Size]byte) error {
	current, err := planDigest(path)
	if err != nil {
		return err
	}
	if current != recorded {
		err := fmt.Errorf("the recorded plan %s changed after planning; refusing to apply it", path)
		slog.Error("tofu.mutation.explicit_plan_changed", "plan", path, "err", err)
		return err
	}
	return nil
}

// acquireExplicitLocks takes one lock set for the apply. The returned context
// ends when a lock is lost. The caller calls release after the apply.
func acquireExplicitLocks(
	ctx context.Context, hosts []hostlock.Host,
) (locked context.Context, scope *hostlock.Scope, release func(), err error) {
	set, run, err := acquireLockSet(ctx, hosts)
	if err != nil {
		return nil, nil, nil, err
	}
	scope, err = set.Scope()
	if err != nil {
		set.ReleaseAll(context.WithoutCancel(ctx))
		slog.Error("lock.scope_failed", "run", run, "err", err)
		return nil, nil, nil, fmt.Errorf("create the host lock scope: %w", err)
	}
	lockedCtx, release := keepLocksRenewed(ctx, set)
	return lockedCtx, scope, release, nil
}
