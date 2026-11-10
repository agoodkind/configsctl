package statemigration

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"goodkind.io/configsctl/internal/migrationpause"
)

type pauseRecord struct {
	Manifest  string `json:"manifest"`
	Directory string `json:"directory"`
}

func (e Engine) acquirePauseLease() (func(), error) {
	if e.session != nil {
		if err := e.session.validate(e); err != nil {
			return nil, err
		}
		return func() {}, nil
	}
	release, err := migrationpause.AcquireExclusive(e.PauseFile)
	if err != nil {
		slog.Error("migration.pause.lease_failed", "path", e.PauseFile, "err", migrationErrorType(err))
		return nil, fmt.Errorf("acquire the migration pause lease: %w", err)
	}
	return release, nil
}

func (e Engine) pause() error {
	slog.Info("migration.pause.requested", "path", e.PauseFile)
	if _, err := os.Lstat(e.PauseFile); err == nil {
		return e.requirePause()
	} else if !errors.Is(err, os.ErrNotExist) {
		slog.Error("migration.pause.inspect_failed", "path", e.PauseFile, "err", migrationErrorType(err))
		return fmt.Errorf("inspect the infrastructure write pause: %w", err)
	}
	fingerprint, err := e.Manifest.fingerprint()
	if err != nil {
		return err
	}
	data, err := json.Marshal(pauseRecord{Manifest: fingerprint, Directory: e.Directory})
	if err != nil {
		slog.Error("migration.pause.encode_failed", "path", e.PauseFile, "err", migrationErrorType(err))
		return fmt.Errorf("encode the infrastructure write pause: %w", err)
	}
	root, err := privateDirectory(filepath.Dir(e.PauseFile))
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := os.CreateTemp(filepath.Dir(e.PauseFile), "pause-next-")
	if err != nil {
		slog.Error("migration.pause.create_failed", "path", e.PauseFile, "err", migrationErrorType(err))
		return fmt.Errorf("create the complete infrastructure write pause: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		slog.Error("migration.pause.write_failed", "path", e.PauseFile, "err", migrationErrorType(err))
		return fmt.Errorf("write the infrastructure pause record: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		slog.Error("migration.pause.sync_failed", "path", e.PauseFile, "err", migrationErrorType(err))
		return fmt.Errorf("sync the infrastructure pause record: %w", err)
	}
	if err := file.Close(); err != nil {
		slog.Error("migration.pause.close_failed", "path", e.PauseFile, "err", migrationErrorType(err))
		return fmt.Errorf("close the infrastructure pause record: %w", err)
	}
	if err := os.Link(file.Name(), e.PauseFile); errors.Is(err, os.ErrExist) {
		return e.requirePause()
	} else if err != nil {
		slog.Error("migration.pause.publish_failed", "path", e.PauseFile, "err", migrationErrorType(err))
		return fmt.Errorf("publish the complete infrastructure pause without replacing another owner: %w", err)
	}
	return syncDirectory(filepath.Dir(e.PauseFile))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		slog.Error("migration.directory_sync.open_failed", "path", path, "err", migrationErrorType(err))
		return fmt.Errorf("open migration directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		slog.Error("migration.directory_sync.failed", "path", path, "err", migrationErrorType(err))
		return fmt.Errorf("sync migration directory: %w", err)
	}
	return nil
}

func (e Engine) requirePause() error {
	data, err := os.ReadFile(e.PauseFile)
	if err != nil {
		slog.Error("migration.pause.read_failed", "path", e.PauseFile, "err", migrationErrorType(err))
		return fmt.Errorf("read the required infrastructure write pause: %w", err)
	}
	fingerprint, err := e.Manifest.fingerprint()
	if err != nil {
		return err
	}
	var pause pauseRecord
	if err := json.Unmarshal(data, &pause); err != nil || pause.Manifest != fingerprint || pause.Directory != e.Directory {
		return errors.New("another migration owns the infrastructure write pause")
	}
	return nil
}
