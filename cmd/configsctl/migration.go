package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"goodkind.io/configsctl/internal/statemigration"
)

const migrationUsage = "usage: configsctl migrate-state <inspect|inspect-plan|prepare|preflight|execute|resume|verify|restore|verify-restored> <workspace-or-manifest> <private-directory>"

type migrationAction string

const (
	migrationInspect        migrationAction = "inspect"
	migrationInspectPlan    migrationAction = "inspect-plan"
	migrationPrepare        migrationAction = "prepare"
	migrationPreflight      migrationAction = "preflight"
	migrationExecute        migrationAction = "execute"
	migrationResume         migrationAction = "resume"
	migrationVerify         migrationAction = "verify"
	migrationRestore        migrationAction = "restore"
	migrationVerifyRestored migrationAction = "verify-restored"
)

func runStateMigration(_ cmdEnv, args []string) error {
	if len(args) != 3 {
		return errors.New(migrationUsage)
	}
	slog.Info("migration.command.started", "action", args[0])
	passwordFile, err := vaultPassPath()
	if err != nil {
		return err
	}
	directory, err := migrationDirectory(args[2])
	if err != nil {
		return err
	}
	action := migrationAction(args[0])
	if action == migrationInspect || action == migrationInspectPlan {
		loaded, err := loadSettings(settingsFile)
		if err != nil {
			return err
		}
		if action == migrationInspectPlan {
			return inspectMigrationPlan(context.Background(), loaded, args[1], passwordFile, directory)
		}
		return inspectMigrationWorkspace(context.Background(), loaded, args[1], passwordFile, directory)
	}
	data, err := os.ReadFile(args[1])
	if err != nil {
		slog.Error("migration.manifest.read_failed", "path", args[1], "err", fmt.Sprintf("%T", err))
		return fmt.Errorf("read the reviewed migration manifest: %w", err)
	}
	manifest, err := statemigration.DecodeManifest(data)
	if err != nil {
		return fmt.Errorf("validate the reviewed migration manifest: %w", err)
	}
	ctx := context.Background()
	engine, workspacesDirectory, err := bindMigrationEngine(ctx, manifest, directory, passwordFile, migrationAction(args[0]))
	if err != nil {
		return err
	}
	if err := runMigrationAction(ctx, engine, workspacesDirectory, migrationAction(args[0])); err != nil {
		return err
	}
	fmt.Printf("State migration %s completed. Private verification records: %s\n", args[0], directory)
	return nil
}

func runMigrationAction(ctx context.Context, engine statemigration.Engine, workspacesDirectory string, action migrationAction) error {
	var err error
	switch action {
	case migrationPrepare:
		err = engine.Prepare(ctx)
	case migrationPreflight:
		err = engine.Preflight(ctx, engine.Manifest.ConfigurationDirectory, workspacesDirectory)
	case migrationExecute, migrationResume:
		err = engine.Execute(ctx)
	case migrationVerify:
		err = engine.Verify(ctx)
	case migrationRestore:
		err = engine.Restore(ctx)
	case migrationVerifyRestored:
		err = engine.VerifyRestored(ctx)
	case migrationInspect, migrationInspectPlan:
		return errors.New(migrationUsage)
	default:
		return errors.New(migrationUsage)
	}
	if err != nil {
		slog.Error("migration.command.failed", "action", action, "err", fmt.Sprintf("%T", err))
		return fmt.Errorf("state migration %s: %w", action, err)
	}
	return nil
}

func bindMigrationEngine(ctx context.Context, manifest statemigration.Manifest, directory, passwordFile string, action migrationAction) (statemigration.Engine, string, error) {
	revision, err := migrationRevision(ctx)
	if err != nil {
		return statemigration.Engine{}, "", err
	}
	configuration, err := reviewedMigrationConfiguration(ctx, manifest.ConfigurationDirectory, manifest.Commit)
	if err != nil {
		return statemigration.Engine{}, "", err
	}
	if _, err := migrationDirectoryAt(directory, configuration); err != nil {
		return statemigration.Engine{}, "", err
	}
	loaded, err := loadSettings(filepath.Join(configuration, settingsFile))
	if err != nil {
		return statemigration.Engine{}, "", err
	}
	workspacesDirectory := loaded.Tofu.WorkspacesDir
	workspaces, err := bindMigrationWorkspaces(configuration, loaded, manifest, passwordFile, false)
	if err != nil {
		return statemigration.Engine{}, "", err
	}
	originals, err := bindOriginalMigrationWorkspaces(ctx, loaded.Tofu.ProviderMirror, manifest, directory, passwordFile)
	if err != nil {
		return statemigration.Engine{}, "", err
	}
	pause, err := migrationPausePath()
	if err != nil {
		return statemigration.Engine{}, "", err
	}
	engine := statemigration.Engine{
		Manifest: manifest, CurrentCommit: revision, Directory: directory, PauseFile: pause,
		Workspaces: workspaces, OriginalWorkspaces: originals, RestoreWorkspaces: nil,
	}
	if action != migrationVerifyRestored {
		return engine, workspacesDirectory, nil
	}
	if revision != manifest.RestoreCommit {
		return statemigration.Engine{}, "", errors.New("restore verification requires the reviewed original configuration commit")
	}
	original, err := filepath.Abs(".")
	if err != nil {
		slog.Error("migration.configuration.original_resolve_failed", "err", fmt.Sprintf("%T", err))
		return statemigration.Engine{}, "", fmt.Errorf("resolve the original configuration checkout: %w", err)
	}
	original, err = filepath.EvalSymlinks(original)
	if err != nil {
		slog.Error("migration.configuration.original_resolve_failed", "err", fmt.Sprintf("%T", err))
		return statemigration.Engine{}, "", fmt.Errorf("resolve the original checkout directory: %w", err)
	}
	loaded, err = loadSettingsWithMirror(filepath.Join(original, settingsFile), loaded.Tofu.ProviderMirror)
	if err != nil {
		return statemigration.Engine{}, "", err
	}
	engine.RestoreWorkspaces, err = bindMigrationWorkspaces(original, loaded, manifest, passwordFile, true)
	if err != nil {
		return statemigration.Engine{}, "", err
	}
	return engine, workspacesDirectory, nil
}

func bindOriginalMigrationWorkspaces(ctx context.Context, mirror *string, manifest statemigration.Manifest, directory, passwordFile string) (map[string]statemigration.Workspace, error) {
	configuration, err := reviewedMigrationConfiguration(ctx, manifest.RestoreConfigurationDirectory, manifest.RestoreCommit)
	if err != nil {
		slog.ErrorContext(ctx, "migration.configuration.original_binding_failed", "err", fmt.Sprintf("%T", err))
		return nil, fmt.Errorf("verify the retained original configuration checkout: %w", err)
	}
	if _, err := migrationDirectoryAt(directory, configuration); err != nil {
		return nil, err
	}
	loaded, err := loadSettingsWithMirror(filepath.Join(configuration, settingsFile), mirror)
	if err != nil {
		return nil, err
	}
	workspaces := make(map[string]statemigration.Workspace)
	for _, spec := range manifest.Workspaces {
		if spec.Before.Lineage == "" {
			continue
		}
		workspace, identity, err := migrationWorkspaceAt(configuration, loaded, spec.Selector, passwordFile)
		if err != nil {
			return nil, err
		}
		if identity != spec.Backend {
			return nil, fmt.Errorf("original workspace %s does not match its reviewed backend identity", spec.Selector)
		}
		workspaces[spec.Selector] = workspace
	}
	return workspaces, nil
}

func bindMigrationWorkspaces(configuration string, loaded settings, manifest statemigration.Manifest, passwordFile string, originalOnly bool) (map[string]statemigration.Workspace, error) {
	workspaces := make(map[string]statemigration.Workspace)
	for _, spec := range manifest.Workspaces {
		if originalOnly && spec.Before.Lineage == "" {
			continue
		}
		workspace, identity, err := migrationWorkspaceAt(configuration, loaded, spec.Selector, passwordFile)
		if err != nil {
			return nil, err
		}
		if identity != spec.Backend {
			return nil, fmt.Errorf("workspace %s does not match its reviewed backend identity", spec.Selector)
		}
		workspaces[spec.Selector] = workspace
	}
	return workspaces, nil
}

func migrationDirectory(path string) (string, error) {
	return migrationDirectoryAt(path, ".")
}

func migrationDirectoryAt(path, configuration string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		slog.Error("migration.directory.resolve_failed", "err", fmt.Sprintf("%T", err))
		return "", fmt.Errorf("resolve the private migration directory: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("resolve the existing parent of the private migration directory: %w", err)
	}
	absolute = filepath.Join(parent, filepath.Base(absolute))
	root, err := filepath.Abs(configuration)
	if err != nil {
		return "", fmt.Errorf("resolve the configuration checkout: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve the real configuration checkout: %w", err)
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil {
		return "", fmt.Errorf("compare the migration directory with the checkout: %w", err)
	}
	if filepath.IsLocal(relative) {
		return "", errors.New("the private migration directory must be outside the configuration checkout")
	}
	return absolute, nil
}

func inspectMigrationWorkspace(ctx context.Context, loaded settings, selector, passwordFile, directory string) (result error) {
	slog.Info("migration.inspection.started", "workspace", selector)
	workspace, identity, err := migrationWorkspace(loaded, selector, passwordFile)
	if err != nil {
		return err
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		slog.Error("migration.inspection.directory_failed", "path", directory, "err", fmt.Sprintf("%T", err))
		return fmt.Errorf("create a new private inspection directory: %w", err)
	}
	plaintext := filepath.Join(directory, "transient.tfstate")
	defer func() {
		if err := os.Remove(plaintext); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove decrypted inspection state: %w", err))
		}
	}()
	info, err := statemigration.Pull(ctx, workspace, plaintext)
	if errors.Is(err, statemigration.ErrAbsent) {
		info = statemigration.EmptyState()
		err = nil
	}
	if err != nil {
		return err
	}
	if info.Lineage != "" {
		if _, err := statemigration.Snapshot(ctx, workspace, plaintext, filepath.Join(directory, "encrypted")); err != nil {
			return fmt.Errorf("encrypt the inspected source state: %w", err)
		}
	}
	metadata, err := json.MarshalIndent(statemigration.WorkspaceSpec{Selector: selector, Backend: identity, Before: info}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode nonsecret state inspection metadata: %w", err)
	}
	file := filepath.Join(directory, "inventory.json")
	if err := os.WriteFile(file, metadata, 0o600); err != nil {
		return fmt.Errorf("write state verification metadata: %w", err)
	}
	fmt.Printf("State verification metadata: %s\n", file)
	return nil
}
