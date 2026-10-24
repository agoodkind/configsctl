package statemigration

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

func cleanupInterruptedFiles(root *os.Root, directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		slog.Error("migration.interrupted_files.list_failed", "path", directory, "err", migrationErrorType(err))
		return fmt.Errorf("list interrupted migration files: %w", err)
	}
	for _, entry := range entries {
		if !transientDirectory(entry.Name()) {
			continue
		}
		private, err := privateDirectory(filepath.Join(directory, entry.Name()))
		if err != nil {
			return err
		}
		if err := private.Close(); err != nil {
			slog.Error("migration.interrupted_files.close_failed", "path", directory, "err", migrationErrorType(err))
			return fmt.Errorf("close interrupted migration directory: %w", err)
		}
		if err := root.RemoveAll(entry.Name()); err != nil {
			slog.Error("migration.interrupted_files.remove_failed", "path", directory, "err", migrationErrorType(err))
			return fmt.Errorf("remove interrupted decrypted migration files: %w", err)
		}
	}
	return nil
}

func transientDirectory(name string) bool {
	for _, prefix := range []string{"prepare-", "write-", "inspect-", "restore-prepare-", "restore-write-", "restore-check-", "preflight-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
