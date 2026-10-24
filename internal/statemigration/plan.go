package statemigration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
)

// CheckPlan refuses native plans that would change managed infrastructure.
// Output updates and data-source cache changes do not modify infrastructure.
func CheckPlan(ctx context.Context, workspace Workspace, directory string) (result error) {
	slog.InfoContext(ctx, "migration.independent_plan.started", "workspace", workspace.Directory)
	scratch, err := os.MkdirTemp(directory, "inspect-plan-")
	if err != nil {
		slog.ErrorContext(ctx, "migration.independent_plan.directory_failed", "workspace", workspace.Directory, "err", migrationErrorType(err))
		return fmt.Errorf("create private independent plan directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			slog.ErrorContext(ctx, "migration.independent_plan.cleanup_failed", "workspace", workspace.Directory, "err", migrationErrorType(err))
			result = errors.Join(result, fmt.Errorf("remove private independent plan files: %w", err))
		}
	}()
	plan := filepath.Join(scratch, "verification.tfplan")
	err = native(ctx, workspace, io.Discard, "plan", "-input=false", "-detailed-exitcode", "-no-color", "-out="+plan)
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 2) {
		return err
	}
	file, err := createPrivate(filepath.Join(scratch, "plan.json"))
	if err != nil {
		return err
	}
	runErr := native(ctx, workspace, file, "show", "-json", "-plan="+plan)
	closeErr := file.Close()
	if runErr != nil || closeErr != nil {
		slog.ErrorContext(ctx, "migration.independent_plan.capture_failed", "workspace", workspace.Directory, "err", migrationErrorType(errors.Join(runErr, closeErr)))
		return errors.Join(runErr, closeErr)
	}
	contents, err := os.ReadFile(file.Name())
	if err != nil {
		slog.ErrorContext(ctx, "migration.independent_plan.read_failed", "workspace", workspace.Directory, "err", migrationErrorType(err))
		return fmt.Errorf("read private independent plan: %w", err)
	}
	var document struct {
		Format  string `json:"format_version"`
		Changes []struct {
			Address string `json:"address"`
			Mode    string `json:"mode"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(contents, &document); err != nil || document.Format != "1.2" {
		return errors.New("the independent plan requires supported native JSON format 1.2")
	}
	for _, change := range document.Changes {
		if change.Mode == "data" {
			continue
		}
		if change.Mode != "managed" || len(change.Change.Actions) == 0 {
			return errors.New("the independent plan contains an unsupported resource change")
		}
		if slices.ContainsFunc(change.Change.Actions, func(action string) bool { return action != "no-op" && action != "read" }) {
			return fmt.Errorf("the independent plan would change managed resource %s", change.Address)
		}
	}
	return nil
}

// Initialize binds native commands to the backend declared by the reviewed root.
func Initialize(ctx context.Context, workspace Workspace) error {
	return native(ctx, workspace, io.Discard, "init", "-reconfigure", "-input=false", "-no-color")
}
