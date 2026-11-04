package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"goodkind.io/configsctl/hostlock"
	"goodkind.io/configsctl/internal/tofulock"
	"goodkind.io/configsctl/tofurun"
)

type nativeConfigurationPlan struct {
	PlannedValues struct {
		Outputs map[string]struct {
			Value tofulock.Targets `json:"value"`
		} `json:"outputs"`
	} `json:"planned_values"`
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
	loaded, err := loadSettings(settingsFile)
	if err != nil {
		return loaded, "", nil, err
	}
	directory, arguments, err := selectTofuWorkspace(loaded.Tofu.WorkspacesDir, args)
	if err != nil {
		return loaded, "", nil, err
	}
	if len(arguments) == 0 {
		return loaded, "", nil, fmt.Errorf("%s", tofuUsage)
	}
	loaded.Tofu, err = loaded.Tofu.withNativeConfiguration(ctx, directory)
	return loaded, directory, arguments, err
}

// evaluateNativeConfiguration evaluates only the selected provider-free module.
// Initialization and planning receive the same constant workspace selector.
func evaluateNativeConfiguration(ctx context.Context, tofu tofuSettings, workspaceDir string) (tofulock.Targets, error) {
	repository, err := os.Getwd()
	if err != nil {
		slog.Error("tofu.configuration.repository_failed", "err", err)
		return tofulock.Targets{}, fmt.Errorf("resolve the configuration repository: %w", err)
	}
	return evaluateNativeConfigurationAt(ctx, tofu, repository, workspaceDir)
}

func evaluateNativeConfigurationAt(ctx context.Context, tofu tofuSettings, repository, workspaceDir string) (tofulock.Targets, error) {
	var targets tofulock.Targets
	if tofu.ConfigurationRoot == "" {
		return targets, fmt.Errorf("tofu.configuration_root is required for native configuration")
	}
	if err := validateWorkspaceSelector(tofu.ConfigurationRoot); err != nil {
		return targets, err
	}
	root, err := repositoryDirectory(filepath.Join(repository, tofu.ConfigurationRoot))
	if err != nil {
		return targets, err
	}
	selector, err := filepath.Rel(tofu.WorkspacesDir, workspaceDir)
	if err != nil {
		return targets, fmt.Errorf("resolve the native workspace selector: %w", err)
	}
	selector = filepath.ToSlash(selector)
	if err := validateWorkspaceSelector(selector); err != nil {
		return targets, err
	}
	binary, err := tofuBinary()
	if err != nil {
		return targets, err
	}
	directory, err := os.MkdirTemp("", "configsctl-configuration-*")
	if err != nil {
		return targets, fmt.Errorf("create the native configuration directory: %w", err)
	}
	defer removePrivateDirectory(directory)
	home := filepath.Join(directory, "home")
	temporary := filepath.Join(directory, "tmp")
	data := filepath.Join(directory, "data")
	for _, path := range []string{home, temporary, data} {
		if err := makePrivateDirectory(path); err != nil {
			return targets, err
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
		request.Args = args
		result, err := tofurun.Run(ctx, request, nil)
		if err != nil {
			return targets, fmt.Errorf("evaluate native configuration for %s: %w", selector, err)
		}
		if result.ExitCode != 0 && (args[0] != "plan" || result.ExitCode != tofuPendingExitCode) {
			return targets, fmt.Errorf("evaluate native configuration for %s: tofu exited with status %d; output: %s", selector, result.ExitCode, result.LogPath)
		}
	}
	request.Args = []string{"show", "-json", plan}
	request.OutputMode = tofurun.PrivateJSON
	request.JSONPath = filepath.Join(temporary, "configuration.json")
	result, err := tofurun.Run(ctx, request, nil)
	if err != nil {
		return targets, fmt.Errorf("read native configuration for %s: %w", selector, err)
	}
	if result.ExitCode != 0 {
		return targets, fmt.Errorf("read native configuration for %s: tofu exited with status %d", selector, result.ExitCode)
	}
	body, err := os.ReadFile(request.JSONPath)
	if err != nil {
		return targets, fmt.Errorf("read the native configuration projection: %w", err)
	}
	var document nativeConfigurationPlan
	if err := json.Unmarshal(body, &document); err != nil {
		return targets, fmt.Errorf("decode the native configuration projection: %w", err)
	}
	projection, found := document.PlannedValues.Outputs["lock_targets"]
	if !found {
		return targets, fmt.Errorf("native configuration for %s must export lock_targets", selector)
	}
	targets = projection.Value
	if err := targets.Validate(); err != nil {
		slog.Error("tofu.configuration.targets_invalid", "workspace", selector, "err", err)
		return targets, fmt.Errorf("validate native lock targets for %s: %w", selector, err)
	}
	if targets.LockEndpoints == nil && (len(targets.ModuleHosts)+len(targets.ModuleKeyHosts)+len(targets.NodeHosts)+len(targets.GuestHosts) > 0) {
		return targets, fmt.Errorf("native configuration for %s must export lock_endpoints", selector)
	}
	return targets, nil
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
	targets, err := evaluateNativeConfiguration(ctx, t, workspace)
	if err != nil {
		return t, err
	}
	t.LockTargets = map[string]tofulock.Targets{tofuWorkspaceKey(workspace): targets}
	if err := validateEndpointCredentials(t); err != nil {
		return t, err
	}
	return t, nil
}
