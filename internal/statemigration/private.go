package statemigration

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

const (
	privateDirectoryMode = 0o700
	privateFileMode      = 0o600
)

func privateDirectory(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("the migration directory must be an absolute path outside the repository")
	}
	err := os.Mkdir(path, privateDirectoryMode)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		slog.Error("migration.private_directory.create_failed", "path", path, "err", migrationErrorType(err))
		return nil, fmt.Errorf("create private migration directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		slog.Error("migration.private_directory.inspect_failed", "path", path, "err", migrationErrorType(err))
		return nil, fmt.Errorf("inspect private migration directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != privateDirectoryMode {
		return nil, errors.New("the migration directory must be a real directory with mode 0700")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		slog.Error("migration.private_directory.open_failed", "path", path, "err", migrationErrorType(err))
		return nil, fmt.Errorf("open private migration directory: %w", err)
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = root.Close()
		return nil, errors.New("the migration directory changed while opening it")
	}
	if err := requirePrivateOwner(opened); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

func createPrivate(path string) (*os.File, error) {
	root, err := privateDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.OpenFile(filepath.Base(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		slog.Error("migration.private_file.create_failed", "path", path, "err", migrationErrorType(err))
		return nil, fmt.Errorf("create private migration file: %w", err)
	}
	return file, nil
}

func writePrivate(path string, data []byte) error {
	file, err := createPrivate(path)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		slog.Error("migration.private_file.write_failed", "path", path, "err", migrationErrorType(err))
		return fmt.Errorf("write private migration file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		slog.Error("migration.private_file.sync_failed", "path", path, "err", migrationErrorType(err))
		return fmt.Errorf("sync private migration file: %w", err)
	}
	if err := file.Close(); err != nil {
		slog.Error("migration.private_file.close_failed", "path", path, "err", migrationErrorType(err))
		return fmt.Errorf("close private migration file: %w", err)
	}
	return nil
}

func protectNativeFiles(directory string) error {
	root, err := privateDirectory(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := os.ReadDir(directory)
	if err != nil {
		slog.Error("migration.native_files.list_failed", "path", directory, "err", migrationErrorType(err))
		return fmt.Errorf("list private native state files: %w", err)
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("the private native state directory contains a symbolic link")
		}
		if entry.IsDir() {
			continue
		}
		if err := root.Chmod(entry.Name(), privateFileMode); err != nil {
			slog.Error("migration.native_files.protect_failed", "path", directory, "err", migrationErrorType(err))
			return fmt.Errorf("protect native state file: %w", err)
		}
	}
	return nil
}

// migrationErrorType excludes messages that can contain state or provider output.
func migrationErrorType(err error) string {
	return fmt.Sprintf("%T", err)
}
