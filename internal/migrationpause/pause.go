// Package migrationpause prevents ordinary writes during a coordinated state transfer.
package migrationpause

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// Path selects the private shared pause for Configs deployments on this controller.
func Path() (string, error) {
	config, err := os.UserConfigDir()
	if err != nil {
		slog.Error("migration.pause.directory_unavailable", "err", err)
		return "", fmt.Errorf("find the private configuration directory for migration recovery: %w", err)
	}
	return filepath.Join(config, "configsctl", "state-migration", "pause.json"), nil
}

// Guard refuses writes until the owning migration verifies the complete state set.
func Guard(pauseFile string) error {
	if _, err := os.Lstat(pauseFile); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		slog.Error("migration.pause.inspect_failed", "path", pauseFile, "err", err)
		return fmt.Errorf("inspect the durable infrastructure write pause: %w", err)
	}
	return errors.New("infrastructure writes remain paused for state migration; resume or restore the owning migration")
}
