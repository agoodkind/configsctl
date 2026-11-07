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
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"goodkind.io/configsctl/hostlock"
	"goodkind.io/configsctl/internal/dockerbridge"
	"goodkind.io/configsctl/internal/redact"
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
	selectedWorkspaceFile      = "environment"
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
	if len(request.args) == 0 {
		return none, false
	}
	targets, declared := request.lockTargets[tofuWorkspaceKey(request.workspaceDir)]
	if !declared || (len(targets.LockEndpoints) == 0 && len(targets.LockFreeTypes) == 0) {
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
	targets, declared := tofu.LockTargets[workspace]
	if !declared || (len(targets.LockEndpoints) == 0 && len(targets.LockFreeTypes) == 0) {
		return fmt.Errorf("workspace %s requires native lock_endpoints or lock_free_types; configure tofu.configuration_root", workspace)
	}
	if !strictExplicitSettings(tofu, targets) {
		return nil
	}
	reason := explicitRefusalReason(tofu, targets, args, command)
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

func strictExplicitSettings(tofu tofuSettings, targets tofulock.Targets) bool {
	return tofu.ConfigurationRoot == "" && slices.ContainsFunc(slices.Collect(maps.Values(targets.LockEndpoints)), func(endpoint tofulock.LockEndpoint) bool { return !endpoint.Local })
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
func runEndpointHelp(ctx context.Context, env cmdEnv, tofu tofuSettings, workspaceDir string, args, childEnv []string) (bool, error) {
	help := tofuargs.BooleanArgument(args, "help", false) || tofuargs.BooleanArgument(args, "h", false)
	if !help || !tofuMutates(args) {
		return false, nil
	}
	if len(tofu.LockTargets[tofuWorkspaceKey(workspaceDir)].LockEndpoints) == 0 {
		return false, nil
	}
	return true, executeTofu(ctx, env, workspaceDir, args, args, childEnv)
}

// validateEndpointCredentials requires the credential host in the declared endpoints.
func validateEndpointCredentials(tofu tofuSettings) error {
	for _, workspace := range slices.Sorted(maps.Keys(tofu.LockTargets)) {
		if len(tofu.LockTargets[workspace].LockEndpoints) == 0 {
			continue
		}
		credential, configured := tofu.Credentials[workspace]
		if !configured {
			continue
		}
		if _, declared := tofu.LockTargets[workspace].LockEndpoints[credential.LockHost]; !declared {
			return fmt.Errorf("workspace %s must declare lock_endpoints for credential host %s", workspace, credential.LockHost)
		}
	}
	return nil
}

func explicitRefusalReason(tofu tofuSettings, targets tofulock.Targets, args []string, command tofuargs.Command) string {
	switch {
	case command == tofuargs.Destroy:
		return "the explicit lock path runs apply only, and destroy would use the inventory lock path"
	case tofu.GuestModules == nil && slices.ContainsFunc(slices.Collect(maps.Values(targets.LockEndpoints)), func(endpoint tofulock.LockEndpoint) bool { return !endpoint.Local }):
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
	return runPlannedMutation(ctx, settings, request, targets, passwordFile, false)
}

func runNativeMutation(ctx context.Context, settings tofuSettings, request tofuLockRequest, targets tofulock.Targets, passwordFile string) error {
	return runPlannedMutation(ctx, settings, request, targets, passwordFile, true)
}

func runPlannedMutation(ctx context.Context, settings tofuSettings, request tofuLockRequest, targets tofulock.Targets, passwordFile string, native bool) error {
	_, _, err := mutationCommands(request.args, "", native)
	if err != nil {
		return err
	}
	ctx, credentialLocks, releaseCredential, err := prepareExplicitCredential(ctx, settings, &request, targets, passwordFile)
	if err != nil {
		return err
	}
	defer releaseCredential()
	runner, cleanup, err := newExplicitRunner(settings, request, passwordFile)
	if err != nil {
		return err
	}
	defer cleanup()
	planArgs, applyArgs, err := mutationCommands(request.args, runner.planPath, native)
	if err != nil {
		return err
	}
	changed, err := prepareMutationPlan(ctx, runner, request, planArgs)
	if err != nil {
		return err
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
	if changed && !tofuargs.AutoApprove(request.args) {
		if err := confirmMutation(ctx, request.args, request.env); err != nil {
			return err
		}
	}
	names := lockHostNames(hosts)
	slog.Info("tofu.mutation.explicit_lock_hosts", "workspace", request.workspaceDir, "hosts", names)
	names = slices.Compact(slices.Sorted(slices.Values(append(names, request.requiredHosts...))))
	lockedCtx, scope, release, err := lockPlannedMutation(ctx, hosts, credentialLocks)
	if err != nil {
		return err
	}
	defer release()
	if err := requireCurrentInstalledSource(lockedCtx, request.workspaceDir); err != nil {
		return err
	}
	if err := requirePlanDigest(runner.planPath, plannedDigest); err != nil {
		return err
	}
	applyResult, err := runner.run(lockedCtx, "apply", applyArgs, tofurun.RedactedProgress, "", scope, names)
	if err != nil {
		return err
	}
	if applyResult.ExitCode != 0 {
		return tofuFailure(lockedCtx, &exitCodeError{code: applyResult.ExitCode}, applyResult.LogPath)
	}
	return nil
}

func mutationCommands(args []string, path string, native bool) ([]string, []string, error) {
	if native {
		return nativeMutationArguments(args, path)
	}
	options, err := parseExplicitArguments(args)
	if err != nil {
		return nil, nil, err
	}
	return explicitPlanArguments(options, path), explicitApplyArguments(options, path), nil
}

func prepareMutationPlan(ctx context.Context, runner explicitRunner, request tofuLockRequest, args []string) (bool, error) {
	if candidate := savedPlanCandidate(request.args); candidate != "" {
		source := workspacePath(request.workspaceDir, candidate)
		if err := copyPrivateFile(source, runner.planPath); err != nil {
			return false, err
		}
		return false, requireTofuPlanSource(ctx, request.workspaceDir, runner.planPath)
	}
	var source tofuPlanSource
	if installedSourceFrom(ctx) != nil {
		var err error
		source, err = inspectTofuPlanSource(ctx, request.workspaceDir)
		if err != nil {
			return false, err
		}
	}
	result, err := runner.run(ctx, "plan", args, tofurun.RedactedProgress, "", nil, nil)
	if err != nil {
		return false, err
	}
	changed := result.ExitCode == tofuPendingExitCode
	if result.ExitCode != 0 && !changed {
		return false, tofuFailure(ctx, &exitCodeError{code: result.ExitCode}, result.LogPath)
	}
	if installedSourceFrom(ctx) != nil {
		if err := recordTofuPlanSource(ctx, request.workspaceDir, runner.planPath, source); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func prepareExplicitCredential(ctx context.Context, settings tofuSettings, request *tofuLockRequest, targets tofulock.Targets, passwordFile string) (context.Context, *hostlock.Set, func(), error) {
	credential, configured := settings.Credentials[tofuWorkspaceKey(request.workspaceDir)]
	if !configured {
		return ctx, nil, func() {}, nil
	}
	decision := tofulock.Decision{AllHosts: false, Reason: "", Hosts: slices.Sorted(maps.Keys(targets.LockEndpoints)), GuestLocks: nil}
	hosts, err := targets.LockHosts(decision)
	if err != nil {
		slog.Error("tofu.credential.lock_targets_failed", "workspace", request.workspaceDir, "err", err)
		return nil, nil, nil, fmt.Errorf("select credential lock targets: %w", err)
	}
	locked, set, release, err := acquireRenewedLocks(ctx, hosts)
	if err != nil {
		return nil, nil, nil, err
	}
	scope, err := set.Scope()
	if err != nil {
		release()
		slog.Error("tofu.credential.scope_failed", "workspace", request.workspaceDir, "err", err)
		return nil, nil, nil, fmt.Errorf("create the credential lock scope: %w", err)
	}
	if err := scope.Check([]string{credential.LockHost}); err != nil {
		release()
		slog.Error("tofu.credential.scope_failed", "workspace", request.workspaceDir, "err", err)
		return nil, nil, nil, fmt.Errorf("check the credential lock scope: %w", err)
	}
	if err := requireCurrentInstalledSource(locked, request.workspaceDir); err != nil {
		release()
		return nil, nil, nil, err
	}
	var patterns []redact.Pattern
	request.env, patterns, err = prepareMutationCredential(locked, credential, request.workspaceDir, request.env, passwordFile)
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	restore, err := installRedaction(patterns)
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	return locked, set, func() { restore(); release() }, nil
}

func lockPlannedMutation(ctx context.Context, hosts []hostlock.Host, existing *hostlock.Set) (context.Context, *hostlock.Scope, func(), error) {
	if existing == nil {
		return acquireExplicitLocks(ctx, hosts)
	}
	if err := existing.AcquireAdditional(ctx, hosts); err != nil {
		slog.Error("tofu.credential.additional_locks_failed", "err", err)
		return nil, nil, nil, fmt.Errorf("acquire the selected mutation locks: %w", err)
	}
	scope, err := existing.Scope()
	if err != nil {
		slog.Error("tofu.credential.scope_failed", "err", err)
		return nil, nil, nil, fmt.Errorf("create the selected mutation lock scope: %w", err)
	}
	return ctx, scope, func() {}, nil
}

// parseExplicitArguments accepts the options of "apply" that the public
// runner can run, each in the -name=value form. It refuses every other
// argument before OpenTofu starts.
func parseExplicitArguments(args []string) (explicitOptions, error) {
	options := explicitOptions{parallelism: explicitDefaultParallelism, lockTimeout: ""}
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
	// directory is the private run directory that stores the Docker bridge sockets.
	directory     string
	dockerBridges map[string]dockerbridge.Bridge
}

// newExplicitRunner creates the private directory and the request that every
// command of the mutation shares. The caller calls cleanup to remove the
// directory.
//
// The request passes OpenTofu only PATH, LANG when set, TF_IN_AUTOMATION, the
// private TMPDIR, the provider mirror configuration path, and declared inputs.
// Native execution retains the bounded operator HOME, optional agent socket,
// and explicitly supplied private OpenTofu data directory.
// Static remote-endpoint execution retains its private HOME.
func newExplicitRunner(settings tofuSettings, request tofuLockRequest, passwordFile string) (explicitRunner, func(), error) {
	var runner explicitRunner
	assignments, err := tofuSecretEnv(settings, request.workspaceDir, defaultVaultFile, passwordFile)
	if err != nil {
		return runner, nil, err
	}
	if credential, configured := settings.Credentials[tofuWorkspaceKey(request.workspaceDir)]; configured {
		rotator := tofuVariablePrefix + credential.Dell.RotatorPasswordKey
		prepared := make([]string, 0, len(assignments)+2)
		for _, assignment := range assignments {
			name, _, _ := strings.Cut(assignment, "=")
			if name != rotator && name != tofuVariablePrefix+credential.UsernameVariable && name != tofuVariablePrefix+credential.PasswordVariable {
				prepared = append(prepared, assignment)
			}
		}
		for _, name := range []string{credential.UsernameVariable, credential.PasswordVariable} {
			key := tofuVariablePrefix + name
			value := environmentValue(request.env, key)
			if value == "" {
				return runner, nil, fmt.Errorf("prepared deployment credential %s is missing", name)
			}
			prepared = append(prepared, key+"="+value)
		}
		assignments = prepared
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
	manual := !strictExplicitSettings(settings, request.lockTargets[tofuWorkspaceKey(request.workspaceDir)])
	workspace, dataDirectory, err := selectedMutationWorkspace(settings, request, root)
	if err != nil {
		return runner, nil, err
	}
	if settings.source != nil {
		root = settings.source.path(root)
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
	inputs := operatorInputs{home: home, assignments: assignments, publicNames: nil}
	if manual {
		inputs, err = manualSourceOperatorInputs(settings, request, assignments)
		if err != nil {
			cleanup()
			return runner, nil, err
		}
	}
	environment := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + inputs.home, "TMPDIR=" + temporary, "TF_IN_AUTOMATION=1",
	}
	if language := os.Getenv("LANG"); language != "" {
		environment = append(environment, "LANG="+language)
	}
	required, redacted := splitExplicitAssignments(inputs.assignments)
	redacted = slices.DeleteFunc(redacted, func(name string) bool { return slices.Contains(inputs.publicNames, name) })
	if credential, configured := settings.Credentials[tofuWorkspaceKey(request.workspaceDir)]; configured {
		username := tofuVariablePrefix + credential.UsernameVariable
		redacted = slices.DeleteFunc(redacted, func(name string) bool { return name == username })
		required = append(required, username)
	}
	runner = explicitRunner{
		base: tofurun.Request{
			Binary: binary, Root: root, Workspace: workspace, DataDirectory: dataDirectory,
			ManualHome: manual, OptionalEnvironment: inputs.publicNames,
			Args: nil, Environment: append(environment, inputs.assignments...),
			RequiredEnvironment: required, SecretEnvironment: redacted, SecretFiles: nil,
			OutputMode: tofurun.RedactedProgress, JSONPath: "", RequiredHosts: nil,
		},
		planPath:  filepath.Join(temporary, mutationPlanName),
		jsonPath:  filepath.Join(temporary, explicitPlanJSONName),
		directory: directory, dockerBridges: settings.dockerBridges,
	}
	return runner, cleanup, nil
}

func selectedMutationWorkspace(settings tofuSettings, request tofuLockRequest, root string) (string, string, error) {
	directory, err := prepareNativeDataDirectory(settings, request.workspaceDir, request.env, request.args)
	if err != nil {
		return "", "", err
	}
	workspace, err := selectedTofuWorkspace(root, directory)
	if err != nil {
		return "", "", err
	}
	if err := requireWorkspaceMatch(request.env, workspace); err != nil {
		return "", "", err
	}
	return workspace, directory, nil
}

func prepareNativeDataDirectory(settings tofuSettings, workspace string, environment, args []string) (string, error) {
	if strictExplicitSettings(settings, settings.LockTargets[tofuWorkspaceKey(workspace)]) {
		return "", nil
	}
	directory := environmentValue(environment, "TF_DATA_DIR")
	if directory == "" {
		if settings.source == nil {
			return "", nil
		}
		directory = ".terraform"
	}
	if !filepath.IsAbs(directory) {
		directory = filepath.Join(workspace, directory)
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		slog.Error("tofu.native.data_directory_failed", "workspace", workspace, "err", err)
		return "", fmt.Errorf("resolve the native OpenTofu data directory: %w", err)
	}
	if tofuargs.Subcommand(args) == "init" && !tofuCommandHelp(args) {
		if err := os.MkdirAll(directory, explicitDirectoryMode); err != nil {
			slog.Error("tofu.native.data_directory_failed", "directory", directory, "err", err)
			return "", fmt.Errorf("create the native OpenTofu data directory: %w", err)
		}
	}
	if err := tofurun.CheckDataDirectory(directory); err != nil {
		slog.Error("tofu.native.data_directory_failed", "directory", directory, "err", err)
		return "", fmt.Errorf("validate the native OpenTofu data directory: %w", err)
	}
	if settings.source != nil {
		selected, err := selectedTofuWorkspace(workspace, directory)
		if err != nil {
			return "", err
		}
		if requested := environmentValue(environment, "TF_WORKSPACE"); requested != "" {
			selected = requested
		}
		if err := settings.source.verified.RequireCommandBackend(workspace, selected, settings.source.copyRoot, args); err != nil {
			return "", fmt.Errorf("validate installed source state storage: %w", err)
		}
	}
	return directory, nil
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

// selectedTofuWorkspace reads the selected workspace from the data directory.
// OpenTofu uses the default workspace without the environment file.
func selectedTofuWorkspace(root, dataDirectory string) (string, error) {
	if dataDirectory == "" {
		dataDirectory = filepath.Join(root, ".terraform")
	}
	body, err := os.ReadFile(filepath.Join(dataDirectory, selectedWorkspaceFile))
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
	if err := requireInstalledModuleCache(ctx, r.base.Root, r.base.DataDirectory, args); err != nil {
		return tofurun.Result{}, err
	}
	request := r.base
	request.Args = args
	request.OutputMode = mode
	request.JSONPath = jsonPath
	request.RequiredHosts = requiredHosts
	bridges := r.dockerBridges
	bridged, getOnly := dockerBridgeAccess(args)
	if !bridged {
		bridges = nil
	}
	var result tofurun.Result
	err := runWithDockerBridges(ctx, bridges, r.directory, request.Environment, getOnly, func(bridgeContext context.Context, bridgeArguments func([]string) []string) error {
		request.Args = bridgeArguments(args)
		var runErr error
		result, runErr = runExplicitStep(bridgeContext, step, request, scope)
		return runErr
	})
	return result, err
}

func runExplicitStep(ctx context.Context, step string, request tofurun.Request, scope *hostlock.Scope) (tofurun.Result, error) {
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
	lockedCtx, set, release, err := acquireRenewedLocks(ctx, hosts)
	if err != nil {
		return nil, nil, nil, err
	}
	scope, err = set.Scope()
	if err != nil {
		release()
		slog.Error("lock.scope_failed", "err", err)
		return nil, nil, nil, fmt.Errorf("create the host lock scope: %w", err)
	}
	return lockedCtx, scope, release, nil
}

func acquireRenewedLocks(ctx context.Context, hosts []hostlock.Host) (context.Context, *hostlock.Set, func(), error) {
	set, _, err := acquireLockSet(ctx, hosts)
	if err != nil {
		return nil, nil, nil, err
	}
	lockedCtx, release := keepLocksRenewed(ctx, set)
	return lockedCtx, set, release, nil
}
