package main

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"goodkind.io/configsctl/internal/migrationpause"
)

func migrationPausePath() (string, error) {
	path, err := migrationpause.Path()
	if err != nil {
		slog.Error("migration.pause.path_failed", "err", err)
		return "", fmt.Errorf("select the infrastructure write pause: %w", err)
	}
	return path, nil
}

func acquireMigrationWriter() (func(), error) {
	path, err := migrationPausePath()
	if err != nil {
		return nil, err
	}
	release, err := migrationpause.AcquireWriter(path)
	if err != nil {
		slog.Error("migration.pause.lease_failed", "err", err)
		return nil, fmt.Errorf("acquire the infrastructure writer lease: %w", err)
	}
	return release, nil
}

func leaseTofuMigration(args []string, usesCredential bool) (func(), error) {
	if !tofuWritesState(args) && !usesCredential {
		return func() {}, nil
	}
	return acquireMigrationWriter()
}

func tofuWritesState(args []string) bool {
	command := string(tofuSubcommand(args))
	if slices.Contains([]string{"apply", "destroy", "refresh", "import", "test", "taint", "untaint"}, command) {
		return true
	}
	if command == "init" {
		for _, argument := range args {
			if slices.Contains([]string{"migrate-state", "force-copy"}, strings.TrimLeft(strings.SplitN(argument, "=", 2)[0], "-")) {
				return true
			}
		}
	}
	if command == "state" && len(args) > 1 && !slices.Contains([]string{"list", "ls", "pull", "show"}, args[1]) {
		return true
	}
	if (command == "workspace" || command == "env") && len(args) > 1 && slices.Contains([]string{"new", "delete"}, args[1]) {
		return true
	}
	return false
}
