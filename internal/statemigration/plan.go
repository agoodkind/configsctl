package statemigration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
)

// CheckPlan refuses native plans that would change managed infrastructure.
// Output updates and data-source cache changes do not modify infrastructure.
func CheckPlan(ctx context.Context, workspace Workspace, directory string) (result error) {
	target := planReportTarget{Directory: directory, Name: "verification-" + digest([]byte(workspace.Directory)) + ".json", Workspace: workspace.Directory}
	return checkPlan(ctx, workspace, directory, target)
}

func checkPlan(ctx context.Context, workspace Workspace, directory string, target planReportTarget) (result error) {
	slog.InfoContext(ctx, "migration.independent_plan.started", "workspace", workspace.Directory)
	report := newPlanReport(target.Workspace)
	defer func() {
		if err := target.Write(report); err != nil {
			result = errors.Join(result, err)
		}
		if result != nil {
			result = fmt.Errorf("%w; private plan metadata: %s", result, target.path())
		}
	}()
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
	if err := writePrivate(plan, nil); err != nil {
		return err
	}
	events, err := createPrivate(filepath.Join(scratch, "events.jsonl"))
	if err != nil {
		return err
	}
	runErr := native(ctx, workspace, events, "plan", "-json", "-input=false", "-detailed-exitcode", "-out="+plan)
	if err := errors.Join(events.Close(), protectNativeFiles(scratch)); err != nil {
		return err
	}
	diagnostics, err := readPlanDiagnostics(events.Name(), workspace.Directory)
	report.Diagnostics = diagnostics
	if err != nil {
		return err
	}
	var exit *exec.ExitError
	if runErr != nil && (!errors.As(runErr, &exit) || exit.ExitCode() != 2) {
		report.Status = "native_plan_failed"
		return runErr
	}
	contents, err := readNativePlan(ctx, workspace, scratch, plan)
	if err != nil {
		return err
	}
	return report.inspect(contents)
}

func readNativePlan(ctx context.Context, workspace Workspace, scratch, plan string) ([]byte, error) {
	slog.InfoContext(ctx, "migration.independent_plan.read_started", "workspace", workspace.Directory)
	file, err := createPrivate(filepath.Join(scratch, "plan.json"))
	if err != nil {
		return nil, err
	}
	runErr := native(ctx, workspace, file, "show", "-json", "-plan="+plan)
	closeErr := file.Close()
	if runErr != nil || closeErr != nil {
		slog.ErrorContext(ctx, "migration.independent_plan.capture_failed", "workspace", workspace.Directory, "err", migrationErrorType(errors.Join(runErr, closeErr)))
		return nil, errors.Join(runErr, closeErr)
	}
	contents, err := os.ReadFile(file.Name())
	if err != nil {
		slog.ErrorContext(ctx, "migration.independent_plan.read_failed", "workspace", workspace.Directory, "err", migrationErrorType(err))
		return nil, fmt.Errorf("read private independent plan: %w", err)
	}
	return contents, nil
}

// Initialize binds native commands to the backend declared by the reviewed root.
func Initialize(ctx context.Context, workspace Workspace) error {
	return native(ctx, workspace, io.Discard, "init", "-reconfigure", "-input=false", "-no-color")
}
