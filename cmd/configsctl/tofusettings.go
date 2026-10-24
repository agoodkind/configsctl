package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"goodkind.io/configsctl/internal/tofulock"
	"gopkg.in/yaml.v3"
)

const settingsFile = "configsctl.yml"

const tofuVariablePrefix = "TF_VAR_"

type settings struct {
	Tofu   tofuSettings   `yaml:"tofu"`
	Deploy deploySettings `yaml:"deploy"`
}

type deploySettings struct {
	LockHostVariables map[string][]string `yaml:"lock_host_variables"`
	// AbsentGuestNodes maps the second label of a host name to the
	// ansible_proxmox_host value of its Proxmox node.
	AbsentGuestNodes map[string]string `yaml:"absent_guest_nodes"`
}

type tofuSettings struct {
	// WorkspacesDir is a path relative to the repository root.
	WorkspacesDir string `yaml:"workspaces_dir"`
	// tofuSecretEnv exports a vault key <EnvKeyPrefix><NAME> as the
	// environment variable <NAME>.
	EnvKeyPrefix string                      `yaml:"env_key_prefix"`
	LockTargets  map[string]tofulock.Targets `yaml:"lock_targets"`
	// GuestModule specifies the module address indexed by service_name for guest plans.
	// An empty GuestModule disables the deploy plan gate.
	GuestModule  string                            `yaml:"guest_module"`
	GuestModules map[string]string                 `yaml:"guest_modules"`
	Credentials  map[string]tofuCredentialSettings `yaml:"credentials"`
}

func (t tofuSettings) guestWorkspaceDir() string {
	return filepath.Join(t.WorkspacesDir, guestWorkspaceName)
}

func (t tofuSettings) guestHosts() map[string]string {
	if t.GuestModules == nil {
		return t.LockTargets[tofuWorkspaceKey(t.guestWorkspaceDir())].GuestHosts
	}
	hosts := make(map[string]string)
	for _, target := range t.LockTargets {
		for _, host := range target.GuestHosts {
			hosts[host] = host
		}
	}
	return hosts
}

func loadSettings(path string) (settings, error) {
	var loaded settings
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("settings file read failed", "path", path, "err", err)
		return loaded, fmt.Errorf("read %s: %w", path, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&loaded); err != nil {
		slog.Error("settings file parse failed", "path", path, "err", err)
		return loaded, fmt.Errorf("parse %s: %w", path, err)
	}
	if loaded.Tofu.WorkspacesDir == "" {
		return loaded, errors.New(path + ": tofu.workspaces_dir is empty")
	}
	if loaded.Tofu.EnvKeyPrefix == "" {
		return loaded, errors.New(path + ": tofu.env_key_prefix is empty")
	}
	if err := validateLockTargets(path, loaded.Tofu.LockTargets); err != nil {
		return loaded, err
	}
	if err := validateTofuCredentials(loaded.Tofu.Credentials); err != nil {
		return loaded, err
	}
	if loaded.Tofu.GuestModule != "" && loaded.Tofu.guestHosts() == nil {
		return loaded, fmt.Errorf(
			"%s: tofu.guest_module requires tofu.lock_targets.%s.guest_hosts",
			path, tofuWorkspaceKey(loaded.Tofu.guestWorkspaceDir()),
		)
	}
	if err := validateGuestPlans(loaded.Tofu); err != nil {
		return loaded, err
	}
	return loaded, validateDeploySettings(path, loaded.Deploy)
}

func validateLockTargets(path string, lockTargets map[string]tofulock.Targets) error {
	if lockTargets != nil && len(lockTargets) == 0 {
		return errors.New(path + ": tofu.lock_targets is empty")
	}
	for _, workspace := range slices.Sorted(maps.Keys(lockTargets)) {
		if workspace == "" {
			return errors.New(path + ": tofu.lock_targets has an empty workspace path")
		}
		if tofuWorkspaceKey(workspace) != workspace {
			return fmt.Errorf("%s: tofu.lock_targets.%s is not a clean slash-separated path", path, workspace)
		}
		if err := lockTargets[workspace].Validate(); err != nil {
			slog.Error("settings lock targets invalid", "path", path, "workspace", workspace, "err", err)
			return fmt.Errorf("%s: tofu.lock_targets.%s: %w", path, workspace, err)
		}
	}
	return nil
}

func validateDeploySettings(path string, deploy deploySettings) error {
	for _, playbook := range slices.Sorted(maps.Keys(deploy.LockHostVariables)) {
		variables := deploy.LockHostVariables[playbook]
		if playbook == "" {
			return errors.New(path + ": deploy.lock_host_variables has an empty playbook name")
		}
		if len(variables) == 0 {
			return fmt.Errorf("%s: deploy.lock_host_variables.%s is empty", path, playbook)
		}
		if slices.Contains(variables, "") {
			return fmt.Errorf("%s: deploy.lock_host_variables.%s has an empty variable name", path, playbook)
		}
	}
	for _, label := range slices.Sorted(maps.Keys(deploy.AbsentGuestNodes)) {
		if label == "" {
			return errors.New(path + ": deploy.absent_guest_nodes has an empty host name label")
		}
		if deploy.AbsentGuestNodes[label] == "" {
			return fmt.Errorf("%s: deploy.absent_guest_nodes.%s has an empty node name", path, label)
		}
	}
	return nil
}

func tofuWorkspaceKey(workspaceDir string) string {
	return filepath.ToSlash(filepath.Clean(workspaceDir))
}
