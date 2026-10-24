package statemigration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
)

type baselinePlanStage struct {
	Original bool
	Current  bool
	Name     string
}

func (e Engine) capturePlanSet(ctx context.Context, current journal, stage baselinePlanStage) (proof []planEntryProof, result error) {
	slog.InfoContext(ctx, "migration.plan_baseline.capture_started", "stage", stage.Name)
	scratch, err := os.MkdirTemp(e.Directory, "preflight-")
	if err != nil {
		return nil, migrationPreflightFailure(ctx, "create private baseline plan files", err)
	}
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			result = errors.Join(result, migrationPreflightFailure(ctx, "remove private baseline plan files", err))
		}
	}()
	configuration, commit := e.Manifest.ConfigurationDirectory, e.Manifest.Commit
	endpoints := e.Workspaces
	if stage.Original {
		configuration, commit = e.Manifest.RestoreConfigurationDirectory, e.Manifest.RestoreCommit
		endpoints = e.OriginalWorkspaces
		if stage.Current {
			configuration, err = e.currentOriginalConfiguration()
			if err != nil {
				return nil, err
			}
			endpoints = e.RestoreWorkspaces
		}
	}
	checkout := filepath.Join(scratch, "configuration")
	if err := archiveMigrationCommit(ctx, configuration, commit, checkout); err != nil {
		return nil, err
	}
	proof = []planEntryProof{}
	var failures []error
	for index, spec := range e.Manifest.Workspaces {
		if stage.Original && spec.Before.Lineage == "" {
			continue
		}
		endpoint, exists := endpoints[spec.Selector]
		if !exists || endpoint.Directory == "" {
			failures = append(failures, fmt.Errorf("baseline workspace %s lacks its reviewed configuration and protected environment", spec.Selector))
			continue
		}
		target := planReportTarget{Directory: e.Directory, Name: fmt.Sprintf("%s-%04d.json", stage.Name, index), Workspace: spec.Selector}
		source := e.baselineSource(current, stage, index, spec)
		contents, err := e.shadowPlan(ctx, scratch, checkout, configuration, endpoint, source, target)
		if err != nil {
			failures = append(failures, migrationPreflightFailure(ctx, "check prepared workspace "+spec.Selector, err))
			continue
		}
		entries, err := e.planProof(contents, spec.Selector, stage.Original)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		proof = append(proof, entries...)
	}
	sortPlanProof(proof)
	if len(failures) != 0 {
		slog.ErrorContext(ctx, "migration.plan_baseline.capture_failed", "stage", stage.Name, "err", migrationErrorType(failures[0]))
	}
	return proof, errors.Join(failures...)
}

func (e Engine) baselineSource(current journal, stage baselinePlanStage, index int, spec WorkspaceSpec) shadowPlanSource {
	state := current.States[spec.Selector]
	if stage.Current {
		if stage.Original {
			state = current.Recovery[spec.Selector]
		}
		return shadowPlanSource{Workspace: e.Workspaces[spec.Selector], Expected: state.After, Snapshot: false}
	}
	name, expected := "after", state.Prepared
	if stage.Original {
		name, expected = "before", state.Before
	}
	workspace := Workspace{Directory: e.snapshotDirectory(name, index), Environment: e.Workspaces[spec.Selector].Environment}
	return shadowPlanSource{Workspace: workspace, Expected: expected, Snapshot: true}
}

func (e Engine) establishBaseline(ctx context.Context, current *journal) error {
	if err := writePlanComparison(e.Directory, "baseline", "incomplete", nil); err != nil {
		return err
	}
	original, originalErr := e.capturePlanSet(ctx, *current, baselinePlanStage{Original: true, Current: false, Name: "baseline-original"})
	cutover, cutoverErr := e.capturePlanSet(ctx, *current, baselinePlanStage{Original: false, Current: false, Name: "preflight"})
	if err := errors.Join(originalErr, cutoverErr); err != nil {
		return err
	}
	if err := e.compareAndReport(original, cutover, "baseline"); err != nil {
		return err
	}
	if err := e.checkFrozenBaseline(ctx, current.Baseline, original, cutover); err != nil {
		return err
	}
	current.Baseline = &planBaseline{Version: 1, Original: original, Cutover: cutover, Forward: true}
	return e.saveJournal(*current)
}

func (e Engine) checkFrozenBaseline(ctx context.Context, baseline *planBaseline, original, cutover []planEntryProof) error {
	if baseline == nil {
		return nil
	}
	if baseline.Version != 1 {
		return errors.New("the native plans differ from the frozen migration baseline")
	}
	if err := e.compareAndReport(baseline.Original, original, "baseline"); err != nil {
		slog.ErrorContext(ctx, "migration.plan_baseline.frozen_original_changed", "err", migrationErrorType(err))
		return fmt.Errorf("the native plans differ from the frozen migration baseline: %w", err)
	}
	if !baseline.Forward {
		return nil
	}
	if err := e.compareAndReport(baseline.Cutover, cutover, "baseline"); err != nil {
		slog.ErrorContext(ctx, "migration.plan_baseline.frozen_cutover_changed", "err", migrationErrorType(err))
		return fmt.Errorf("the native plans differ from the frozen migration baseline: %w", err)
	}
	return nil
}

func (e Engine) verifyPlanBaseline(ctx context.Context, current *journal, restored bool) error {
	if current.Baseline == nil {
		if !restored {
			return errors.New("the migration lacks its verified original and cutover plan baseline")
		}
		original, err := e.capturePlanSet(ctx, *current, baselinePlanStage{Original: true, Current: false, Name: "baseline-original"})
		if err != nil {
			return err
		}
		current.Baseline = &planBaseline{Version: 1, Original: original, Cutover: nil, Forward: false}
		if err := e.saveJournal(*current); err != nil {
			return err
		}
	}
	if current.Baseline.Version != 1 || (!restored && !current.Baseline.Forward) {
		return errors.New("the migration checkpoint lacks the required native plan baseline proof")
	}
	if current.Baseline.Forward && !slices.Equal(current.Baseline.Original, current.Baseline.Cutover) {
		return errors.New("the recorded cutover baseline differs from its frozen original plan")
	}
	name := "verify"
	expected := current.Baseline.Cutover
	if restored {
		name, expected = "verify-restored", current.Baseline.Original
	}
	if err := writePlanComparison(e.Directory, name, "incomplete", nil); err != nil {
		return err
	}
	actual, err := e.capturePlanSet(ctx, *current, baselinePlanStage{Original: restored, Current: true, Name: name})
	if err != nil {
		return err
	}
	if err := e.compareAndReport(expected, actual, name); err != nil {
		slog.ErrorContext(ctx, "migration.plan_baseline.changed", "stage", name, "err", "native_plan_mismatch")
		return fmt.Errorf("the current managed plan differs from the frozen original baseline; the write pause remains active: %w", err)
	}
	return nil
}
