package main

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/configsctl/internal/redact"
	"goodkind.io/configsctl/internal/statemigration"
)

func coordinateMigrationAction(ctx context.Context, engine statemigration.Engine, workspacesDirectory, passwordFile string, action migrationAction) error {
	if action == migrationPrepare || action == migrationRestore {
		return runMigrationAction(ctx, engine, workspacesDirectory, action)
	}
	credentials, err := prepareMigrationCredentials(ctx, &engine, passwordFile)
	if err != nil {
		return err
	}
	if action != migrationTransfer && len(credentials.deployments) == 0 {
		return runMigrationAction(ctx, engine, workspacesDirectory, action)
	}
	patterns := credentials.patterns()
	if key, valid := redact.Validate(patterns); !valid {
		return fmt.Errorf("migration credential %s cannot support protected output", key)
	}
	restore, err := installRedaction(patterns)
	if err != nil {
		return err
	}
	defer restore()
	sessionErr := engine.WithSession(ctx, func(owned statemigration.Engine) error {
		if err := owned.ValidatePlanAction(ctx, string(action)); err != nil {
			return fmt.Errorf("validate the protected migration action: %w", err)
		}
		locked := ctx
		if len(credentials.hosts) != 0 {
			var release func()
			var err error
			locked, release, err = takeLocks(ctx, credentials.hosts)
			if err != nil {
				return err
			}
			defer release()
		}
		for _, deployment := range credentials.deployments {
			if err := deployment.settings.Dell.PrepareCredential(locked, deployment.rotator, deployment.credential); err != nil {
				slog.ErrorContext(locked, "migration.credential.activation_failed", "host", deployment.settings.LockHost, "err", fmt.Sprintf("%T", err))
				return fmt.Errorf("activate the protected migration credential for host %s: %w", deployment.settings.LockHost, err)
			}
		}
		return runMigrationAction(locked, owned, workspacesDirectory, action)
	})
	if sessionErr != nil {
		return fmt.Errorf("run the protected migration session: %w", sessionErr)
	}
	return nil
}
