package statemigration

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

//go:embed configsctl_migration_override.tf
var migrationOverrideTemplate []byte

const migrationOverridePlaceholder = "CONFIGSCTL_MIGRATION_STATE_PATH"

// Preflight compares original and prepared native plans in private reviewed checkouts.
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
	if configurationDirectory != e.Manifest.ConfigurationDirectory {
		return errors.New("preflight requires the manifest's retained cutover configuration checkout")
	}
	if _, err := preflightWorkspaceDirectory(ctx, configurationDirectory, workspacesDirectory); err != nil {
		return err
	}
	current, err := e.loadJournal()
	if err != nil {
		return err
	}
	return e.establishBaseline(ctx, &current)
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
