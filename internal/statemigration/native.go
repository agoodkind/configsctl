package statemigration

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed snapshot.tf
var snapshotConfiguration []byte

// ErrAbsent identifies an initialized backend that does not contain state yet.
var ErrAbsent = errors.New("the initialized backend does not contain a state file")

// Workspace selects one initialized backend and its protected process environment.
type Workspace struct {
	Directory   string
	Environment []string
}

// Pull captures native state bytes directly into a new private file.
// It never writes those bytes through stdout redaction or a command log.
func Pull(ctx context.Context, workspace Workspace, destination string) (StateInfo, error) {
	slog.InfoContext(ctx, "migration.state_pull.started", "workspace", workspace.Directory, "path", destination)
	file, err := createPrivate(destination)
	if err != nil {
		return EmptyState(), err
	}
	runErr := native(ctx, workspace, file, "state", "pull")
	info, statErr := file.Stat()
	syncErr := file.Sync()
	closeErr := file.Close()
	if runErr == nil && statErr == nil && info.Size() == 0 {
		runErr = ErrAbsent
	}
	if runErr != nil {
		if errors.Is(runErr, ErrAbsent) {
			if err := os.Remove(destination); err != nil {
				slog.ErrorContext(ctx, "migration.state_pull.empty_capture_remove_failed", "path", destination, "err", migrationErrorType(err))
				return EmptyState(), fmt.Errorf("remove empty private state capture: %w", err)
			}
		}
		return EmptyState(), runErr
	}
	if syncErr != nil {
		slog.ErrorContext(ctx, "migration.state_pull.sync_failed", "path", destination, "err", migrationErrorType(syncErr))
		return EmptyState(), fmt.Errorf("sync private native state capture: %w", syncErr)
	}
	if statErr != nil {
		slog.ErrorContext(ctx, "migration.state_pull.inspect_failed", "path", destination, "err", migrationErrorType(statErr))
		return EmptyState(), fmt.Errorf("inspect private native state capture: %w", statErr)
	}
	if closeErr != nil {
		slog.ErrorContext(ctx, "migration.state_pull.close_failed", "path", destination, "err", migrationErrorType(closeErr))
		return EmptyState(), fmt.Errorf("close private native state capture: %w", closeErr)
	}
	state, inspectErr := Inspect(destination)
	if inspectErr != nil && nativeAbsentState(destination) {
		if err := os.Remove(destination); err != nil {
			slog.ErrorContext(ctx, "migration.state_pull.empty_capture_remove_failed", "path", destination, "err", migrationErrorType(err))
			return EmptyState(), fmt.Errorf("remove empty private state capture: %w", err)
		}
		return EmptyState(), ErrAbsent
	}
	return state, inspectErr
}

func nativeAbsentState(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(data) {
		return false
	}
	var state struct {
		Version          int                        `json:"version"`
		TerraformVersion string                     `json:"terraform_version"`
		Serial           uint64                     `json:"serial"`
		Lineage          string                     `json:"lineage"`
		Resources        []json.RawMessage          `json:"resources"`
		Outputs          map[string]json.RawMessage `json:"outputs"`
		CheckResults     json.RawMessage            `json:"check_results"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil || state.Version != 4 || state.Lineage != "" || state.Serial != 0 || len(state.Resources) != 0 || len(state.Outputs) != 0 {
		return false
	}
	return len(state.CheckResults) == 0 || bytes.Equal(bytes.TrimSpace(state.CheckResults), []byte("null"))
}

// Move uses native local state operations without applying infrastructure.
func Move(ctx context.Context, workspace Workspace, source, destination, oldAddress, newAddress string) error {
	if err := requireLocalBackend(workspace); err != nil {
		return err
	}
	for _, directory := range []string{filepath.Dir(source), filepath.Dir(destination)} {
		root, err := privateDirectory(directory)
		if err != nil {
			return err
		}
		if err := root.Close(); err != nil {
			slog.ErrorContext(ctx, "migration.state_move.close_failed", "path", directory, "err", migrationErrorType(err))
			return fmt.Errorf("close private state directory: %w", err)
		}
	}
	args := []string{"state", "mv", "-state=" + source}
	if destination != source {
		args = append(args, "-state-out="+destination)
	}
	args = append(args, oldAddress, newAddress)
	runErr := native(ctx, workspace, io.Discard, args...)
	err := errors.Join(runErr, protectNativeFiles(filepath.Dir(source)), protectNativeFiles(filepath.Dir(destination)))
	if err != nil {
		slog.ErrorContext(ctx, "migration.state_move.failed", "workspace", workspace.Directory, "err", migrationErrorType(err))
	}
	return err
}

func requireLocalBackend(workspace Workspace) error {
	data, err := os.ReadFile(filepath.Join(workspace.Directory, ".terraform", "terraform.tfstate"))
	if err != nil {
		slog.Error("migration.local_backend.read_failed", "workspace", workspace.Directory, "err", migrationErrorType(err))
		return fmt.Errorf("read initialized local backend metadata before private state editing: %w", err)
	}
	var initialized struct {
		Backend struct {
			Type string `json:"type"`
		} `json:"backend"`
	}
	if err := json.Unmarshal(data, &initialized); err != nil || initialized.Backend.Type != "local" {
		return errors.New("private state edits require an initialized local backend; remote backends do not honor legacy state paths")
	}
	return nil
}

// Push uses normal backend locking and serial protection without force options.
func Push(ctx context.Context, workspace Workspace, source string) error {
	return native(ctx, workspace, io.Discard, "state", "push", "-lock=true", source)
}

// Snapshot asks the native encrypted local backend to retain an exact state copy.
func Snapshot(ctx context.Context, source Workspace, plaintext, directory string) (Workspace, error) {
	root, err := privateDirectory(directory)
	if err != nil {
		return Workspace{}, err
	}
	if err := root.Close(); err != nil {
		slog.ErrorContext(ctx, "migration.snapshot.close_failed", "path", directory, "err", migrationErrorType(err))
		return Workspace{}, fmt.Errorf("close snapshot directory: %w", err)
	}
	if err := writePrivate(filepath.Join(directory, "main.tf"), snapshotConfiguration); err != nil {
		return Workspace{}, err
	}
	workspace := Workspace{Directory: directory, Environment: source.Environment}
	if err := native(ctx, workspace, io.Discard, "init", "-input=false", "-no-color"); err != nil {
		return Workspace{}, err
	}
	if err := Push(ctx, workspace, plaintext); err != nil {
		return Workspace{}, err
	}
	if err := protectSnapshotFiles(directory); err != nil {
		return Workspace{}, err
	}
	encrypted, err := os.ReadFile(filepath.Join(directory, "snapshot.tfstate"))
	if err != nil {
		slog.ErrorContext(ctx, "migration.snapshot.read_failed", "path", directory, "err", migrationErrorType(err))
		return Workspace{}, fmt.Errorf("read native encrypted snapshot: %w", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(encrypted, &envelope); err != nil || len(envelope["encrypted_data"]) == 0 || len(envelope["resources"]) != 0 {
		return Workspace{}, errors.New("OpenTofu did not write an encrypted snapshot")
	}
	return workspace, nil
}

func protectSnapshotFiles(directory string) error {
	if err := protectNativeFiles(directory); err != nil {
		return err
	}
	root, err := privateDirectory(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Chmod(".terraform", privateDirectoryMode); err != nil {
		slog.Error("migration.snapshot.metadata_directory_protect_failed", "err", migrationErrorType(err))
		return fmt.Errorf("protect retained snapshot backend directory: %w", err)
	}
	if err := root.Chmod(filepath.Join(".terraform", "terraform.tfstate"), privateFileMode); err != nil {
		slog.Error("migration.snapshot.metadata_protect_failed", "err", migrationErrorType(err))
		return fmt.Errorf("protect retained snapshot backend metadata: %w", err)
	}
	return nil
}

func native(ctx context.Context, workspace Workspace, output io.Writer, args ...string) error {
	slog.InfoContext(ctx, "migration.native.started", "workspace", workspace.Directory)
	cmd := exec.CommandContext(ctx, "tofu", args...)
	cmd.Dir = workspace.Directory
	for _, entry := range workspace.Environment {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "TF_CLI_ARGS") || strings.HasPrefix(key, "TF_LOG") ||
			strings.HasPrefix(key, "TOFU_LOG") || key == "TF_TEMP_LOG_PATH" || key == "TOFU_CPU_PROFILE" ||
			key == "TF_DATA_DIR" || key == "TF_WORKSPACE" {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "TF_WORKSPACE=default", "TF_IN_AUTOMATION=true", "TF_INPUT=0", "TF_LOG=OFF")
	cmd.Stdout = output
	// Provider errors can include state attributes. Keep them out of visible logs.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if len(args) == 2 && args[0] == "state" && args[1] == "pull" && strings.Contains(stderr.String(), "No state file was found") {
			return ErrAbsent
		}
		slog.ErrorContext(ctx, "migration.native.failed", "workspace", workspace.Directory, "err", migrationErrorType(err))
		return fmt.Errorf("native OpenTofu %s failed in %s: %w; state values remain private", args[0], workspace.Directory, err)
	}
	return nil
}
