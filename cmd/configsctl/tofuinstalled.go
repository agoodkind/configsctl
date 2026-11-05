package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
	"goodkind.io/configsctl/internal/installedsource"
	"goodkind.io/configsctl/internal/procgroup"
)

type installedTofuSource struct {
	settings        installedsource.Settings
	verified        *installedsource.Source
	copyRoot        string
	trees           []string
	required        []string
	credentialPaths []string
}

type installedSourceKey struct{}

type installedPathOption string

const (
	installedVarFile           installedPathOption = "var-file"
	installedBackendConfig     installedPathOption = "backend-config"
	installedPluginDir         installedPathOption = "plugin-dir"
	installedTestDirectory     installedPathOption = "test-directory"
	installedOut               installedPathOption = "out"
	installedState             installedPathOption = "state"
	installedStateOut          installedPathOption = "state-out"
	installedBackup            installedPathOption = "backup"
	installedJSONInto          installedPathOption = "json-into"
	installedGenerateConfigOut installedPathOption = "generate-config-out"
)

func installedSourceFrom(ctx context.Context) *installedTofuSource {
	source, _ := ctx.Value(installedSourceKey{}).(*installedTofuSource)
	return source
}

func readInstalledTofuSource(ctx context.Context, settings installedsource.Settings) ([]byte, error) {
	target := transport.HostArtifactTarget(settings.Workspace)
	// #nosec G204 -- HostArtifactTarget returns a fixed-length hexadecimal digest.
	command := exec.CommandContext(ctx, "/usr/bin/pvesh", "get", "/nodes/localhost/host-artifacts/"+target, "--output-format", "json")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = os.Stderr
	if err := procgroup.RunGroup(command); err != nil {
		slog.Error("tofu.source.pvesh_failed", "err", err)
		return nil, fmt.Errorf("read the local installed source with pvesh: %w", err)
	}
	return output.Bytes(), nil
}

func prepareInstalledTofuSource(ctx context.Context) (context.Context, func(), error) {
	settingsBytes, err := os.ReadFile(settingsFile)
	if err != nil {
		slog.Error("tofu.source.settings_failed", "err", err)
		return ctx, nil, fmt.Errorf("read %s: %w", settingsFile, err)
	}
	loaded, err := parseSettings(settingsBytes, settingsFile, nil, false)
	if err != nil {
		return ctx, nil, fmt.Errorf("verify installed OpenTofu source: %w", err)
	}
	if loaded.Tofu.InstalledSource == nil {
		return ctx, func() {}, nil
	}
	settings := *loaded.Tofu.InstalledSource
	passwordFile, err := vaultPassPath()
	if err != nil {
		return ctx, nil, err
	}
	credentialPaths := []string{filepath.Join(settings.Root, defaultVaultFile), passwordFile}
	trees := []string{loaded.Tofu.WorkspacesDir}
	if loaded.Tofu.ConfigurationRoot != "" {
		trees = append(trees, loaded.Tofu.ConfigurationRoot)
	}
	for _, tree := range trees {
		if !filepath.IsLocal(tree) || tree == "." {
			return ctx, nil, errors.New("installed source input trees must be relative directories below the installed root")
		}
		for _, credentialPath := range credentialPaths {
			relative, err := filepath.Rel(filepath.Join(settings.Root, tree), credentialPath)
			if err != nil || filepath.IsLocal(relative) {
				return ctx, nil, errors.New("installed source input trees must not include credential files")
			}
		}
	}
	data, err := readInstalledTofuSource(ctx, settings)
	if err != nil {
		return ctx, nil, err
	}
	current, err := os.Getwd()
	if err != nil {
		slog.Error("tofu.source.directory_failed", "err", err)
		return ctx, nil, fmt.Errorf("read the installed working directory: %w", err)
	}
	required := []string{settingsFile}
	verified, err := installedsource.Verify(settings, current, data, trees, required)
	if err != nil {
		return ctx, nil, fmt.Errorf("verify installed OpenTofu inputs: %w", err)
	}
	verifiedSettings, err := verified.Read(settingsFile)
	if err != nil {
		return ctx, nil, fmt.Errorf("verify installed settings bytes: %w", err)
	}
	if !bytes.Equal(settingsBytes, verifiedSettings) {
		return ctx, nil, errors.New("the installed settings changed during source verification")
	}
	required = verified.InputFiles(credentialPaths)
	directory, err := os.MkdirTemp("", explicitDirectoryPattern)
	if err != nil {
		return ctx, nil, fmt.Errorf("create the private installed source directory: %w", err)
	}
	cleanup := func() { removePrivateDirectory(directory) }
	if err := verified.Copy(directory, required); err != nil {
		cleanup()
		return ctx, nil, fmt.Errorf("copy installed OpenTofu inputs: %w", err)
	}
	source := &installedTofuSource{
		settings: settings, verified: verified, copyRoot: directory,
		trees: trees, required: required, credentialPaths: credentialPaths,
	}
	return context.WithValue(ctx, installedSourceKey{}, source), cleanup, nil
}

func (s *installedTofuSource) path(path string) string {
	if !filepath.IsAbs(path) {
		return filepath.Join(s.copyRoot, path)
	}
	relative, err := filepath.Rel(s.settings.Root, path)
	if err == nil && filepath.IsLocal(relative) {
		return filepath.Join(s.copyRoot, relative)
	}
	return path
}

func sourceExecutionPath(ctx context.Context, path string) string {
	if source := installedSourceFrom(ctx); source != nil {
		return source.path(path)
	}
	return path
}

func (s *installedTofuSource) inspect(ctx context.Context) (tofuPlanSource, error) {
	data, err := readInstalledTofuSource(ctx, s.settings)
	if err != nil {
		return tofuPlanSource{}, err
	}
	current, err := os.Getwd()
	if err != nil {
		slog.Error("tofu.source.directory_failed", "err", err)
		return tofuPlanSource{}, fmt.Errorf("read the installed working directory: %w", err)
	}
	verified, err := installedsource.Verify(s.settings, current, data, s.trees, s.required)
	if err != nil {
		return tofuPlanSource{}, fmt.Errorf("recheck installed OpenTofu inputs: %w", err)
	}
	identity := verified.Identity
	if identity != s.verified.Identity {
		return tofuPlanSource{}, errors.New("the installed artifact changed; restart the command from the current installation")
	}
	return tofuPlanSource{
		Status: tofuSourceCommitted, Commit: identity.SourceRevision, Kind: "installed", Workspace: "",
		Root: identity.Root, Artifact: identity.Workspace, Node: identity.Node,
		Environment: identity.Environment, ManifestSHA256: identity.ManifestSHA256,
	}, nil
}

func requireCurrentInstalledSource(ctx context.Context, workspace string) error {
	if installedSourceFrom(ctx) == nil {
		return nil
	}
	_, err := inspectTofuPlanSource(ctx, workspace)
	return err
}

func installedWorkspace(s *installedTofuSource, workspace string) (string, error) {
	absolute := workspace
	if !filepath.IsAbs(absolute) {
		absolute = filepath.Join(s.settings.Root, absolute)
	}
	relative, err := filepath.Rel(s.settings.Root, absolute)
	if err != nil || !filepath.IsLocal(relative) {
		return "", errors.New("the OpenTofu workspace must be inside the installed root")
	}
	return filepath.ToSlash(relative), nil
}

// Check actual file arguments after TF_CLI_ARGS expansion. Output paths and
// mutable state retain their original locations outside the private source copy.
func (s *installedTofuSource) arguments(workspace string, args []string) ([]string, error) {
	result := slices.Clone(args)
	for index := 0; index < len(result); index++ {
		argument := result[index]
		name, value, assigned := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		if !strings.HasPrefix(argument, "-") {
			continue
		}
		if name == "chdir" || name == "from-module" || name == "config" {
			return nil, fmt.Errorf("installed source does not accept -%s", name)
		}
		if !installedFileArgument(name) {
			continue
		}
		valueIndex := index
		if !assigned {
			index++
			if index >= len(result) {
				return nil, fmt.Errorf("OpenTofu option %s requires a value", argument)
			}
			valueIndex = index
			value = result[index]
		}
		if name == "backend-config" && strings.Contains(value, "=") {
			if err := s.requireBackendOverride(workspace, name, value); err != nil {
				return nil, err
			}
			continue
		}
		path, err := s.argumentPath(workspace, name, value)
		if err != nil {
			return nil, err
		}
		if err := s.requireBackendOverride(workspace, name, value); err != nil {
			return nil, err
		}
		if assigned {
			result[valueIndex] = strings.SplitN(argument, "=", 2)[0] + "=" + path
		} else {
			result[valueIndex] = path
		}
	}
	if installedPositionalFile(result) {
		absolute, err := filepath.Abs(workspacePath(workspace, result[len(result)-1]))
		if err != nil {
			slog.Error("tofu.source.argument_path_failed", "err", err)
			return nil, fmt.Errorf("resolve the OpenTofu file argument: %w", err)
		}
		result[len(result)-1] = absolute
	}
	return result, nil
}

func (s *installedTofuSource) requireBackendOverride(workspace, name, value string) error {
	if name != string(installedBackendConfig) {
		return nil
	}
	if err := s.verified.RequireBackendOverride(workspace, value, s.copyRoot); err != nil {
		slog.Error("tofu.source.backend_override_refused", "err", err)
		return fmt.Errorf("validate the installed local backend override: %w", err)
	}
	return nil
}

func installedPositionalFile(args []string) bool {
	if savedPlanCandidate(args) != "" {
		return true
	}
	if len(args) < 2 || strings.HasPrefix(args[len(args)-1], "-") {
		return false
	}
	command, _ := tofuEnvironmentCommand(args)
	return command == "show" || command == "state push"
}

func installedFileArgument(name string) bool {
	switch installedPathOption(name) {
	case installedVarFile, installedBackendConfig, installedPluginDir, installedTestDirectory, installedOut, installedState, installedStateOut, installedBackup, installedJSONInto, installedGenerateConfigOut:
		return true
	default:
		return false
	}
}

func (s *installedTofuSource) argumentPath(workspace, name, value string) (string, error) {
	path := workspacePath(workspace, value)
	switch installedPathOption(name) {
	case installedVarFile, installedBackendConfig:
		return s.copyInput(path)
	case installedPluginDir, installedTestDirectory:
		if err := s.requireNonsecretDirectory(path); err != nil {
			return "", err
		}
		if err := s.verified.AddTree(path); err != nil {
			return "", fmt.Errorf("verify installed OpenTofu input directory: %w", err)
		}
		s.trees = append(s.trees, path)
		if err := s.verified.Copy(s.copyRoot, nil); err != nil {
			return "", fmt.Errorf("copy installed OpenTofu input directory: %w", err)
		}
		return s.path(path), nil
	case installedBackup:
		if value == "-" {
			return value, nil
		}
	case installedOut, installedState, installedStateOut, installedJSONInto, installedGenerateConfigOut:
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		slog.Error("tofu.source.argument_path_failed", "err", err)
		return "", fmt.Errorf("resolve the OpenTofu output path: %w", err)
	}
	return absolute, nil
}

func (s *installedTofuSource) copyInput(path string) (string, error) {
	absolute, err := s.verified.Path(path)
	if err != nil {
		return "", fmt.Errorf("resolve the installed OpenTofu file argument: %w", err)
	}
	if slices.Contains(s.credentialPaths, absolute) {
		return "", errors.New("credential files must retain their original paths and cannot be OpenTofu source arguments")
	}
	if _, err := s.verified.Read(path); err != nil {
		slog.Error("tofu.source.file_argument_failed", "err", err)
		return "", fmt.Errorf("verify the installed OpenTofu file argument: %w", err)
	}
	s.required = append(s.required, path)
	if err := s.verified.Copy(s.copyRoot, []string{path}); err != nil {
		slog.Error("tofu.source.file_argument_copy_failed", "err", err)
		return "", fmt.Errorf("copy the installed OpenTofu file argument: %w", err)
	}
	return s.path(path), nil
}

func (s *installedTofuSource) requireNonsecretDirectory(path string) error {
	absolute, err := s.verified.Path(path)
	if err != nil {
		slog.Error("tofu.source.input_directory_failed", "err", err)
		return fmt.Errorf("resolve the installed OpenTofu input directory: %w", err)
	}
	for _, credentialPath := range s.credentialPaths {
		relative, err := filepath.Rel(absolute, credentialPath)
		if err != nil || filepath.IsLocal(relative) {
			return errors.New("installed source input directories must not include credential files")
		}
	}
	return nil
}

func (s *installedTofuSource) environment(environment []string) ([]string, error) {
	result := slices.Clone(environment)
	for index, assignment := range result {
		name, value, _ := strings.Cut(assignment, "=")
		if name != providerConfigVariable || value == "" {
			continue
		}
		path, err := s.copyInput(value)
		if err != nil {
			return nil, err
		}
		result[index] = name + "=" + path
	}
	return result, nil
}
