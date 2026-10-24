package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"goodkind.io/configsctl/internal/statemigration"
)

func inspectMigrationPlan(ctx context.Context, loaded settings, selector, passwordFile, directory string) error {
	private, err := migrationDirectory(directory)
	if err != nil {
		return err
	}
	workspace, _, err := migrationWorkspace(loaded, selector, passwordFile)
	if err != nil {
		return err
	}
	if err := os.Mkdir(private, 0o700); err != nil {
		slog.ErrorContext(ctx, "migration.plan_inspection.directory_failed", "path", private, "err", fmt.Sprintf("%T", err))
		return fmt.Errorf("create a new private plan inspection directory: %w", err)
	}
	if err := statemigration.Initialize(ctx, workspace); err != nil {
		return fmt.Errorf("initialize the selected plan inspection workspace: %w", err)
	}
	planErr := statemigration.CheckPlan(ctx, workspace, private)
	reports := filepath.Join(private, "reports")
	entries, err := os.ReadDir(reports)
	if err != nil {
		slog.ErrorContext(ctx, "migration.plan_inspection.metadata_read_failed", "path", reports, "err", fmt.Sprintf("%T", err))
		return errors.Join(planErr, fmt.Errorf("read the private plan metadata directory: %w", err))
	}
	if len(entries) != 1 {
		return errors.Join(planErr, errors.New("the plan inspection must retain one private metadata report"))
	}
	fmt.Printf("Private plan metadata: %s\n", filepath.Join(reports, entries[0].Name()))
	if planErr != nil {
		return fmt.Errorf("inspect the selected native plan: %w", planErr)
	}
	return nil
}
