package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"goodkind.io/configsctl/internal/statemigration"
)

type migrationBackendKind string

const (
	migrationLocal migrationBackendKind = "local"
	migrationS3    migrationBackendKind = "s3"
)

func migrationWorkspace(loaded settings, selector, passwordFile string) (statemigration.Workspace, string, error) {
	root, err := filepath.Abs(".")
	if err != nil {
		slog.Error("migration.workspace.root_failed", "err", fmt.Sprintf("%T", err))
		return statemigration.Workspace{}, "", fmt.Errorf("resolve the migration configuration checkout: %w", err)
	}
	return migrationWorkspaceAt(root, loaded, selector, passwordFile)
}

func migrationWorkspaceAt(configurationDirectory string, loaded settings, selector, passwordFile string) (statemigration.Workspace, string, error) {
	directory, err := migrationWorkspaceBase(configurationDirectory, loaded.Tofu.WorkspacesDir)
	if err != nil {
		return statemigration.Workspace{}, "", err
	}
	if selector != "." {
		selected, exists, err := selectedWorkspaceDir(directory, selector)
		if err != nil {
			return statemigration.Workspace{}, "", err
		}
		if !exists {
			return statemigration.Workspace{}, "", fmt.Errorf("migration workspace %s does not exist", selector)
		}
		directory = selected
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		slog.Error("migration.workspace.resolve_failed", "workspace", selector, "err", fmt.Sprintf("%T", err))
		return statemigration.Workspace{}, "", fmt.Errorf("resolve migration workspace: %w", err)
	}
	identity, err := migrationBackend(directory)
	if err != nil {
		return statemigration.Workspace{}, "", err
	}
	environment, err := tofuSecretEnv(loaded.Tofu, directory, filepath.Join(configurationDirectory, defaultVaultFile), passwordFile)
	if err != nil {
		return statemigration.Workspace{}, "", err
	}
	return statemigration.Workspace{Directory: absolute, Environment: append(os.Environ(), environment...)}, identity, nil
}

func migrationWorkspaceBase(configurationDirectory, workspacesDirectory string) (string, error) {
	if !filepath.IsLocal(workspacesDirectory) || filepath.Clean(workspacesDirectory) != workspacesDirectory || strings.Contains(workspacesDirectory, "\\") {
		return "", errors.New("tofu.workspaces_dir must be a clean relative path inside the reviewed configuration checkout")
	}
	directory := configurationDirectory
	if workspacesDirectory == "." {
		return directory, nil
	}
	for component := range strings.SplitSeq(filepath.ToSlash(workspacesDirectory), "/") {
		directory = filepath.Join(directory, component)
		info, err := os.Lstat(directory)
		if err != nil {
			slog.Error("migration.workspace.directory_failed", "path", directory, "err", fmt.Sprintf("%T", err))
			return "", fmt.Errorf("inspect the reviewed workspace directory: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("the reviewed workspace directory must not contain symbolic links")
		}
	}
	return directory, nil
}

func migrationBackend(directory string) (string, error) {
	blocks, err := tofuModuleBlocks(directory, tofuTerraformSchema)
	if err != nil {
		slog.Error("migration.backend.read_failed", "path", directory, "err", fmt.Sprintf("%T", err))
		return "", err
	}
	var backends hcl.Blocks
	for _, block := range blocks {
		content, _, diagnostics := block.Body.PartialContent(tofuBackendSchema)
		if diagnostics.HasErrors() {
			return "", fmt.Errorf("read the migration backend in %s", directory)
		}
		backends = append(backends, content.Blocks...)
	}
	if len(backends) != 1 {
		return "", fmt.Errorf("migration workspace %s requires exactly one explicit backend", directory)
	}
	backend := backends[0]
	attributes, diagnostics := backend.Body.JustAttributes()
	if diagnostics.HasErrors() {
		return "", fmt.Errorf("read migration backend attributes in %s", directory)
	}
	properties := map[string]string{"type": backend.Labels[0]}
	switch migrationBackendKind(backend.Labels[0]) {
	case migrationLocal:
		value, err := migrationBackendString(attributes, "path")
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(directory, value)
		}
		absolute, err := filepath.Abs(value)
		if err != nil {
			return "", fmt.Errorf("resolve local migration backend: %w", err)
		}
		properties["path"] = absolute
	case migrationS3:
		if err := migrationS3Properties(attributes, properties); err != nil {
			return "", err
		}
	default:
		return "", errors.New("state migration supports the configured native local and S3 backends")
	}
	encoded, err := json.Marshal(properties)
	if err != nil {
		return "", fmt.Errorf("encode migration backend identity: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return backend.Labels[0] + ":" + hex.EncodeToString(sum[:]), nil
}

func migrationS3Properties(attributes hcl.Attributes, properties map[string]string) error {
	for _, name := range []string{"bucket", "key"} {
		value, err := migrationBackendString(attributes, name)
		if err != nil {
			return err
		}
		properties[name] = value
	}
	endpoint, exists := attributes["endpoints"]
	if !exists {
		return errors.New("the migration S3 backend requires an explicit endpoint identity")
	}
	value, diagnostics := endpoint.Expr.Value(nil)
	if diagnostics.HasErrors() || !value.IsWhollyKnown() || value.IsNull() || !value.Type().IsObjectType() || !value.Type().HasAttribute("s3") {
		return errors.New("the migration S3 endpoint must be a literal endpoints.s3 string")
	}
	address := value.GetAttr("s3")
	if address.Type() != cty.String || address.IsNull() {
		return errors.New("the migration S3 endpoint must be a literal string")
	}
	properties["endpoint"] = address.AsString()
	return nil
}

func migrationBackendString(attributes hcl.Attributes, name string) (string, error) {
	attribute, exists := attributes[name]
	if !exists {
		return "", fmt.Errorf("the migration backend requires explicit %s", name)
	}
	value, diagnostics := attribute.Expr.Value(nil)
	if diagnostics.HasErrors() || !value.IsWhollyKnown() || value.IsNull() || value.Type() != cty.String || value.AsString() == "" {
		return "", fmt.Errorf("the migration backend requires a literal nonempty %s string", name)
	}
	return value.AsString(), nil
}

func migrationRevision(ctx context.Context) (string, error) {
	return migrationRevisionAt(ctx, ".")
}

func migrationRevisionAt(ctx context.Context, directory string) (string, error) {
	slog.Info("migration.configuration.inspect")
	status := exec.CommandContext(ctx, "git", "status", "--porcelain", "--untracked-files=all")
	status.Dir = directory
	output, err := status.Output()
	if err != nil {
		slog.Error("migration.configuration.status_failed", "err", fmt.Sprintf("%T", err))
		return "", fmt.Errorf("inspect the migration configuration checkout: %w", err)
	}
	if len(output) != 0 {
		return "", errors.New("commit the reviewed configuration changes before preparing a state migration")
	}
	command := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	command.Dir = directory
	output, err = command.Output()
	if err != nil {
		return "", fmt.Errorf("read the migration configuration revision: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func reviewedMigrationConfiguration(ctx context.Context, directory, commit string) (string, error) {
	root, err := filepath.EvalSymlinks(directory)
	if err != nil {
		slog.Error("migration.configuration.resolve_failed", "path", directory, "err", fmt.Sprintf("%T", err))
		return "", fmt.Errorf("resolve the retained cutover checkout: %w", err)
	}
	revision, err := migrationRevisionAt(ctx, root)
	if err != nil {
		return "", err
	}
	if revision != commit {
		return "", errors.New("the retained cutover checkout does not match the reviewed cutover configuration commit")
	}
	command := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		slog.Error("migration.configuration.root_failed", "path", root, "err", fmt.Sprintf("%T", err))
		return "", fmt.Errorf("identify the retained cutover checkout root: %w", err)
	}
	actual, err := filepath.EvalSymlinks(strings.TrimSpace(string(output)))
	if err != nil {
		slog.Error("migration.configuration.root_resolve_failed", "path", root, "err", fmt.Sprintf("%T", err))
		return "", fmt.Errorf("resolve the retained Git checkout root: %w", err)
	}
	if actual != root {
		return "", errors.New("configuration_directory must select the retained cutover Git checkout root")
	}
	return root, nil
}
