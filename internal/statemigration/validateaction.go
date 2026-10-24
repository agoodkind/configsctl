package statemigration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
)

type migrationPlanAction string

const (
	validatePreflight      migrationPlanAction = "preflight"
	validateTransfer       migrationPlanAction = "transfer"
	validateExecute        migrationPlanAction = "execute"
	validateResume         migrationPlanAction = "resume"
	validateVerify         migrationPlanAction = "verify"
	validateVerifyRestored migrationPlanAction = "verify-restored"
)

// ValidatePlanAction checks immutable ownership and journal phase without provider calls.
func (e Engine) ValidatePlanAction(ctx context.Context, action string) error {
	if err := context.Cause(ctx); err != nil {
		slog.ErrorContext(ctx, "migration.action_validation.canceled", "err", migrationErrorType(err))
		return fmt.Errorf("validate the migration session context: %w", err)
	}
	var phases []string
	commit := e.Manifest.Commit
	switch migrationPlanAction(action) {
	case validatePreflight, validateTransfer, validateExecute, validateResume:
		phases = []string{phasePrepared, phaseApplying, phaseTransferred}
	case validateVerify:
		phases = []string{phaseTransferred, phaseVerified}
	case validateVerifyRestored:
		commit = e.Manifest.RestoreCommit
		phases = []string{phaseRestored, phaseRestoredVerified}
	default:
		return errors.New("the migration action does not support protected provider plans")
	}
	slog.InfoContext(ctx, "migration.action_validation.started", "action", action)
	if e.CurrentCommit != commit {
		return errors.New("the migration plan action requires its reviewed configuration commit")
	}
	release, err := e.acquire()
	if err != nil {
		return err
	}
	defer release()
	current, err := e.loadJournal()
	if err != nil {
		return err
	}
	if !slices.Contains(phases, current.Phase) {
		return fmt.Errorf("cannot run migration %s in phase %s", action, current.Phase)
	}
	return e.validatePlanPause(ctx, migrationPlanAction(action))
}

func (e Engine) validatePlanPause(ctx context.Context, action migrationPlanAction) error {
	_, err := os.Lstat(e.PauseFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.ErrorContext(ctx, "migration.action_validation.pause_inspection_failed", "path", e.PauseFile, "err", migrationErrorType(err))
		return fmt.Errorf("inspect the migration pause before provider credential activation: %w", err)
	}
	if err == nil || action == validateVerify || action == validateVerifyRestored {
		return e.requirePause()
	}
	return nil
}
