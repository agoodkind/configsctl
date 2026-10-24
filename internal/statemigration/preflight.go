package statemigration

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

//go:embed configsctl_migration_override.tf
var migrationOverrideTemplate []byte

const migrationOverridePlaceholder = "CONFIGSCTL_MIGRATION_STATE_PATH"

// Preflight checks every prepared state against a private copy of the reviewed commit.
func (e Engine) Preflight(ctx context.Context, configurationDirectory, workspacesDirectory string) (result error) {
	slog.InfoContext(ctx, "migration.preflight.started", "commit", e.Manifest.Commit)
	if e.CurrentCommit != e.Manifest.Commit {
		return errors.New("preflight requires the reviewed cutover configuration commit")
	}
	release, err := e.acquire()
	if err != nil {
		return err
	}
	defer release()
	scratch, err := os.MkdirTemp(e.Directory, "preflight-")
	if err != nil {
		return migrationPreflightFailure(ctx, "create the private preflight directory", err)
	}
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			result = errors.Join(result, migrationPreflightFailure(ctx, "remove private preflight files", err))
		}
	}()
	checkout := filepath.Join(scratch, "configuration")
	if err := archiveMigrationCommit(ctx, configurationDirectory, e.Manifest.Commit, checkout); err != nil {
		return err
	}
	relative, err := preflightWorkspaceDirectory(ctx, configurationDirectory, workspacesDirectory)
	if err != nil {
		return err
	}
	var failures []error
	for index, spec := range e.Manifest.Workspaces {
		endpoint, exists := e.Workspaces[spec.Selector]
		if !exists {
			failures = append(failures, fmt.Errorf("preflight workspace %s lacks its protected environment", spec.Selector))
			continue
		}
		workspace := Workspace{
			Directory: filepath.Join(checkout, relative, spec.Selector), Environment: endpoint.Environment,
		}
		if err := preflightMigrationWorkspace(ctx, e.Directory, scratch, index, workspace); err != nil {
			failures = append(failures, migrationPreflightFailure(ctx, "check prepared workspace "+spec.Selector, err))
		}
	}
	if len(failures) != 0 {
		slog.ErrorContext(ctx, "migration.preflight.workspace_checks_failed", "workspaces", len(failures), "err", fmt.Sprintf("%T", failures[0]))
	}
	return errors.Join(failures...)
}

func preflightWorkspaceDirectory(ctx context.Context, configurationDirectory, workspacesDirectory string) (string, error) {
	relative := workspacesDirectory
	if filepath.IsAbs(workspacesDirectory) {
		configuration, err := filepath.Abs(configurationDirectory)
		if err != nil {
			return "", migrationPreflightFailure(ctx, "resolve the reviewed configuration directory", err)
		}
		relative, err = filepath.Rel(configuration, workspacesDirectory)
		if err != nil {
			return "", migrationPreflightFailure(ctx, "select copied workspaces", err)
		}
	}
	if !filepath.IsLocal(relative) {
		return "", errors.New("preflight workspaces must remain inside the reviewed configuration checkout")
	}
	return relative, nil
}

func preflightMigrationWorkspace(ctx context.Context, migrationDirectory, scratch string, index int, workspace Workspace) error {
	state := filepath.Join(scratch, fmt.Sprintf("shadow-%04d.tfstate", index))
	quoted := hclwrite.TokensForValue(cty.StringVal(state)).Bytes()
	configuration := bytes.ReplaceAll(migrationOverrideTemplate, []byte(migrationOverridePlaceholder), quoted)
	if err := writePreflightOverride(ctx, workspace.Directory, configuration); err != nil {
		return err
	}
	if err := Initialize(ctx, workspace); err != nil {
		return migrationPreflightFailure(ctx, "initialize the private local backend using the configured provider installation", err)
	}
	if err := requirePreflightLocalBackend(ctx, workspace.Directory, state); err != nil {
		return err
	}
	plaintext := filepath.Join(scratch, fmt.Sprintf("prepared-%04d.tfstate", index))
	snapshot := Workspace{
		Directory: filepath.Join(migrationDirectory, fmt.Sprintf("after-%04d", index)), Environment: workspace.Environment,
	}
	if _, err := Pull(ctx, snapshot, plaintext); err != nil {
		return migrationPreflightFailure(ctx, "read the retained encrypted prepared snapshot", err)
	}
	if err := Push(ctx, workspace, plaintext); err != nil {
		return migrationPreflightFailure(ctx, "populate the verified private local backend", err)
	}
	if err := CheckPlan(ctx, workspace, scratch); err != nil {
		return migrationPreflightFailure(ctx, "verify prepared state against the reviewed configuration", err)
	}
	return nil
}

func writePreflightOverride(ctx context.Context, directory string, data []byte) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return migrationPreflightFailure(ctx, "open the copied workspace", err)
	}
	defer root.Close()
	file, err := root.OpenFile("configsctl_migration_override.tf", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return migrationPreflightFailure(ctx, "create the private backend override", err)
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return migrationPreflightFailure(ctx, "write the private backend override", err)
	}
	return nil
}

func requirePreflightLocalBackend(ctx context.Context, directory, statePath string) error {
	data, err := os.ReadFile(filepath.Join(directory, ".terraform", "terraform.tfstate"))
	if err != nil {
		return migrationPreflightFailure(ctx, "read the initialized preflight backend", err)
	}
	var metadata struct {
		Backend struct {
			Type   string `json:"type"`
			Config struct {
				Path string `json:"path"`
			} `json:"config"`
		} `json:"backend"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil || metadata.Backend.Type != "local" || metadata.Backend.Config.Path != statePath {
		return errors.New("the initialized preflight backend does not match the private local state path")
	}
	return nil
}

func migrationPreflightFailure(ctx context.Context, operation string, err error) error {
	slog.ErrorContext(ctx, "migration.preflight.failed", "operation", operation, "err", fmt.Sprintf("%T", err))
	return fmt.Errorf("%s: %w", operation, err)
}
