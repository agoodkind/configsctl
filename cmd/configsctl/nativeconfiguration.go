package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"goodkind.io/configsctl/hostlock"
	"goodkind.io/configsctl/internal/dockerbridge"
	"goodkind.io/configsctl/internal/installedsource"
	"goodkind.io/configsctl/internal/tofuargs"
	"goodkind.io/configsctl/internal/tofulock"
	"goodkind.io/configsctl/tofurun"
)

type nativeConfigurationPlan struct {
	PlannedValues struct {
		Outputs map[string]struct {
			Value json.RawMessage `json:"value"`
		} `json:"outputs"`
	} `json:"planned_values"`
}

// nativeProjection contains the lock targets and Docker bridges of one workspace.
type nativeProjection struct {
	targets       tofulock.Targets
	dockerBridges map[string]dockerbridge.Bridge
}

func nativeLockTarget(ctx context.Context, repository, workspace, host string) (hostlock.Host, error) {
	loaded, err := loadNativeSettings(filepath.Join(repository, settingsFile))
	if err != nil {
		return hostlock.Host{}, err
	}
	if err := validateWorkspaceSelector(workspace); err != nil {
		return hostlock.Host{}, err
	}
	return migrationCredentialLockTarget(ctx, loaded.Tofu, repository, workspace, host)
}

func runNativeLint(ctx context.Context, tofu tofuSettings) error {
	if tofu.ConfigurationRoot == "" {
		return fmt.Errorf("lint requires tofu.configuration_root")
	}
	arguments := []string{"fmt", "-check", "-recursive"}
	environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	if err := executeTofu(ctx, cmdEnv{secrets: nil}, tofu.WorkspacesDir, arguments, arguments, environment); err != nil {
		return err
	}
	workspaces, err := tofuChildWorkspaces(tofu.WorkspacesDir)
	if err != nil {
		return err
	}
	if len(workspaces) == 0 {
		return fmt.Errorf("%s does not contain any native workspaces", tofu.WorkspacesDir)
	}
	for _, workspace := range workspaces {
		operational, err := tofuHasOperationalConfiguration(filepath.Join(tofu.WorkspacesDir, workspace))
		if err != nil {
			return err
		}
		if !operational {
			continue
		}
		if _, err := evaluateNativeConfiguration(ctx, tofu, filepath.Join(tofu.WorkspacesDir, workspace)); err != nil {
			return err
		}
	}
	return nil
}

func loadTofuWorkspace(ctx context.Context, args []string) (settings, string, []string, error) {
	loaded, err := loadNativeSettings(sourceExecutionPath(ctx, settingsFile))
	if err != nil {
		return loaded, "", nil, err
	}
	loaded.Tofu.source = installedSourceFrom(ctx)
	if loaded.Tofu.ProviderMirror != nil {
		mirror, err := resolveProviderMirror(settingsFile, *loaded.Tofu.ProviderMirror)
		if err != nil {
			return loaded, "", nil, err
		}
		loaded.Tofu.ProviderMirror = &mirror
	}
	directory, arguments, err := selectTofuWorkspace(sourceExecutionPath(ctx, loaded.Tofu.WorkspacesDir), args)
	if err != nil {
		return loaded, "", nil, err
	}
	if loaded.Tofu.source != nil {
		directory, err = filepath.Rel(loaded.Tofu.source.copyRoot, directory)
		if err != nil {
			slog.Error("tofu.source.workspace_failed", "err", err)
			return loaded, "", nil, fmt.Errorf("resolve the installed workspace selector: %w", err)
		}
	}
	if len(arguments) == 0 {
		return loaded, "", nil, fmt.Errorf("%s", tofuUsage)
	}
	if loaded.Tofu.source != nil {
		selector := ""
		if loaded.Tofu.ConfigurationRoot != "" {
			selector, err = nativeWorkspaceSelector(loaded.Tofu, directory)
			if err != nil {
				return loaded, "", nil, err
			}
		}
		if err := loaded.Tofu.source.verified.CheckModuleSources(directory, loaded.Tofu.ConfigurationRoot, selector); err != nil {
			return loaded, "", nil, fmt.Errorf("verify installed module selections: %w", err)
		}
	}
	loaded.Tofu, err = loaded.Tofu.withNativeConfiguration(ctx, directory)
	return loaded, directory, arguments, err
}

// evaluateNativeConfiguration evaluates only the selected provider-free module.
// Initialization and planning receive the same constant workspace selector.
func evaluateNativeConfiguration(ctx context.Context, tofu tofuSettings, workspaceDir string) (nativeProjection, error) {
	repository, err := os.Getwd()
	if err != nil {
		slog.Error("tofu.configuration.repository_failed", "err", err)
		return nativeProjection{}, fmt.Errorf("resolve the configuration repository: %w", err)
	}
	return evaluateNativeProjectionAt(ctx, tofu, repository, workspaceDir)
}

func evaluateNativeConfigurationAt(ctx context.Context, tofu tofuSettings, repository, workspaceDir string) (tofulock.Targets, error) {
	projection, err := evaluateNativeProjectionAt(ctx, tofu, repository, workspaceDir)
	return projection.targets, err
}

func evaluateNativeProjectionAt(ctx context.Context, tofu tofuSettings, repository, workspaceDir string) (nativeProjection, error) {
	var projection nativeProjection
	outputs, selector, err := readNativeOutputs(ctx, tofu, repository, workspaceDir)
	if err != nil {
		return projection, err
	}
	encoded, found := outputs["lock_targets"]
	if !found {
		return projection, fmt.Errorf("native configuration for %s must export lock_targets", selector)
	}
	var targets tofulock.Targets
	if err := json.Unmarshal(encoded, &targets); err != nil {
		return projection, fmt.Errorf("decode the native lock targets for %s: %w", selector, err)
	}
	if err := targets.Validate(); err != nil {
		slog.Error("tofu.configuration.targets_invalid", "workspace", selector, "err", err)
		return projection, fmt.Errorf("validate native lock targets for %s: %w", selector, err)
	}
	if targets.LockEndpoints == nil && (len(targets.ModuleHosts)+len(targets.ModuleKeyHosts)+len(targets.NodeHosts)+len(targets.GuestHosts) > 0) {
		return projection, fmt.Errorf("native configuration for %s must export lock_endpoints", selector)
	}
	projection.targets = targets
	bridges, err := decodeDockerBridges(selector, outputs[dockerBridgesOutput], targets)
	if err != nil {
		return projection, err
	}
	variableDirectory := workspaceDir
	if tofu.source != nil {
		variableDirectory = tofu.source.path(workspaceDir)
	}
	if err := requireDockerBridgeVariables(variableDirectory, bridges); err != nil {
		return projection, err
	}
	projection.dockerBridges = bridges
	return projection, nil
}

// readNativeOutputs evaluates the configuration root for the workspace and
// returns each planned output value with the workspace selector.
func readNativeOutputs(ctx context.Context, tofu tofuSettings, repository, workspaceDir string) (map[string]json.RawMessage, string, error) {
	if tofu.ConfigurationRoot == "" {
		return nil, "", fmt.Errorf("tofu.configuration_root is required for native configuration")
	}
	if err := validateWorkspaceSelector(tofu.ConfigurationRoot); err != nil {
		return nil, "", err
	}
	root, err := repositoryDirectory(sourceExecutionPath(ctx, filepath.Join(repository, tofu.ConfigurationRoot)))
	if err != nil {
		return nil, "", err
	}
	selector, err := nativeWorkspaceSelector(tofu, workspaceDir)
	if err != nil {
		return nil, "", err
	}
	binary, err := tofuBinary()
	if err != nil {
		return nil, "", err
	}
	directory, err := os.MkdirTemp("", "configsctl-configuration-*")
	if err != nil {
		slog.Error("tofu.configuration.directory_failed", "err", err)
		return nil, "", fmt.Errorf("create the native configuration directory: %w", err)
	}
	defer removePrivateDirectory(directory)
	home := filepath.Join(directory, "home")
	temporary := filepath.Join(directory, "tmp")
	data := filepath.Join(directory, "data")
	for _, path := range []string{home, temporary, data} {
		if err := makePrivateDirectory(path); err != nil {
			return nil, "", err
		}
	}
	request := tofurun.Request{
		Binary: binary, Root: root, Workspace: defaultTofuWorkspace, DataDirectory: data,
		ProgressToStderr:    true,
		Environment:         []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + temporary, "TF_IN_AUTOMATION=1", "TF_VAR_workspace=" + selector},
		RequiredEnvironment: []string{"TF_VAR_workspace"}, OutputMode: tofurun.RedactedProgress,
	}
	plan := filepath.Join(temporary, "configuration.tfplan")
	for _, args := range [][]string{
		{"init", "-backend=false", "-input=false", "-lockfile=readonly", "-no-color"},
		{"plan", "-refresh=false", "-lock=false", "-input=false", "-no-color", "-detailed-exitcode", "-parallelism=1", "-out=" + plan},
	} {
		if args[0] == "plan" {
			if err := checkNativeResolvedModules(ctx, tofu, binary, data, selector); err != nil {
				return nil, "", err
			}
		}
		request.Args = tofuargs.WithWorkspaceSelector(args, selector)
		result, err := tofurun.Run(ctx, request, nil)
		if err != nil {
			return nil, "", fmt.Errorf("evaluate native configuration for %s: %w", selector, err)
		}
		if result.ExitCode != 0 && (args[0] != "plan" || result.ExitCode != tofuPendingExitCode) {
			return nil, "", fmt.Errorf("evaluate native configuration for %s: tofu exited with status %d; output: %s", selector, result.ExitCode, result.LogPath)
		}
	}
	request.Args = []string{"show", "-json", plan}
	request.OutputMode = tofurun.PrivateJSON
	request.JSONPath = filepath.Join(temporary, "configuration.json")
	result, err := tofurun.Run(ctx, request, nil)
	if err != nil {
		return nil, "", fmt.Errorf("read native configuration for %s: %w", selector, err)
	}
	if result.ExitCode != 0 {
		return nil, "", fmt.Errorf("read native configuration for %s: tofu exited with status %d", selector, result.ExitCode)
	}
	body, err := os.ReadFile(request.JSONPath)
	if err != nil {
		return nil, "", fmt.Errorf("read the native configuration projection: %w", err)
	}
	var document nativeConfigurationPlan
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, "", fmt.Errorf("decode the native configuration projection: %w", err)
	}
	outputs := make(map[string]json.RawMessage, len(document.PlannedValues.Outputs))
	for name, output := range document.PlannedValues.Outputs {
		outputs[name] = output.Value
	}
	return outputs, selector, nil
}

func checkNativeResolvedModules(ctx context.Context, tofu tofuSettings, binary, data, selector string) error {
	if tofu.source == nil {
		return nil
	}
	request := installedsource.ModuleCacheRequest{
		Binary: binary, Workspace: tofu.ConfigurationRoot, CopyRoot: tofu.source.copyRoot,
		DataDirectory: data, WorkspaceSelector: selector, AllowMissing: false,
	}
	if err := tofu.source.verified.CheckModuleCache(ctx, request); err != nil {
		slog.Error("tofu.configuration.module_resolution_refused", "err", err)
		return fmt.Errorf("verify resolved native modules before planning: %w", err)
	}
	return nil
}

func nativeWorkspaceSelector(tofu tofuSettings, workspace string) (string, error) {
	selector, err := filepath.Rel(tofu.WorkspacesDir, workspace)
	if err != nil {
		slog.Error("tofu.configuration.selector_failed", "err", err)
		return "", fmt.Errorf("resolve the native workspace selector: %w", err)
	}
	selector = filepath.ToSlash(selector)
	if err := validateWorkspaceSelector(selector); err != nil {
		return "", err
	}
	return selector, nil
}

func repositoryDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		slog.Error("tofu.configuration.directory_failed", "path", path, "err", err)
		return "", fmt.Errorf("resolve configuration directory %s: %w", path, err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("read configuration directory %s: %w", path, err)
	}
	if canonical != absolute {
		return "", fmt.Errorf("configuration directory %s must not use symbolic links", path)
	}
	return absolute, nil
}

func (t tofuSettings) withNativeConfiguration(ctx context.Context, workspace string) (tofuSettings, error) {
	if t.ConfigurationRoot == "" {
		return t, nil
	}
	projection, err := evaluateNativeConfiguration(ctx, t, workspace)
	if err != nil {
		return t, err
	}
	t.LockTargets = map[string]tofulock.Targets{tofuWorkspaceKey(workspace): projection.targets}
	t.dockerBridges = projection.dockerBridges
	if err := validateEndpointCredentials(t); err != nil {
		return t, err
	}
	return t, nil
}
