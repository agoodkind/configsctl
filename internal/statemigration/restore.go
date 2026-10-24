package statemigration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const phaseRestoredVerified = "restored-verified"

type restoreInstance struct {
	transfer Transfer
	staged   string
}

type duplicateInstance struct {
	workspace string
	file      string
	address   string
}

// Restore reverses the reviewed native transfers without applying infrastructure.
// The durable pause remains active until VerifyRestored checks the original roots.
func (e Engine) Restore(ctx context.Context) error {
	release, err := e.acquire()
	if err != nil {
		return err
	}
	defer release()
	releasePause, err := e.acquirePauseLease()
	if err != nil {
		return err
	}
	defer releasePause()
	current, err := e.loadJournal()
	if err != nil {
		return err
	}
	if !slices.Contains([]string{phasePrepared, phaseApplying, phaseTransferred, phaseRestoring, phaseRestored}, current.Phase) {
		return fmt.Errorf("cannot restore a migration in phase %s", current.Phase)
	}
	if err := e.pause(); err != nil {
		return err
	}
	if err := e.removeRestorationScratch(); err != nil {
		return err
	}
	if current.Recovery == nil {
		if err := e.prepareRestoration(ctx, &current); err != nil {
			return err
		}
	}
	if err := e.checkRestoration(ctx, current, false); err != nil {
		return err
	}
	for index, spec := range e.Manifest.Workspaces {
		if err := e.restoreWorkspace(ctx, index, spec, &current); err != nil {
			return err
		}
	}
	current.Phase = phaseRestored
	return e.saveJournal(current)
}

func (e Engine) prepareRestoration(ctx context.Context, current *journal) (result error) {
	if current.Phase == phaseRestoring || current.Phase == phaseRestored {
		return errors.New("the restoration checkpoint omits its prepared inverse states")
	}
	if err := e.retainOrphanedCandidates(); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp(e.Directory, "restore-prepare-")
	if err != nil {
		slog.Error("migration.restore.prepare_directory_failed", "err", err)
		return fmt.Errorf("create private inverse preparation directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			result = errors.Join(result, fmt.Errorf("remove transient decrypted inverse files: %w", err))
		}
	}()
	actual, files, err := e.captureRestorationBases(ctx, scratch, *current)
	if err != nil {
		return err
	}
	if err := e.reverseTransfers(ctx, scratch, actual, files); err != nil {
		return err
	}
	recovery, err := e.captureRestorationCandidates(ctx, scratch, actual, files)
	if err != nil {
		return err
	}
	current.Recovery = recovery
	current.Phase = phaseRestoring
	current.Restored = nil
	return e.saveJournal(*current)
}

func (e Engine) captureRestorationBases(ctx context.Context, scratch string, current journal) (map[string]StateInfo, map[string]string, error) {
	actual := make(map[string]StateInfo)
	files := make(map[string]string)
	for index, spec := range e.Manifest.Workspaces {
		file := filepath.Join(scratch, stateName(index))
		state, err := Pull(ctx, e.Workspaces[spec.Selector], file)
		if errors.Is(err, ErrAbsent) {
			var absent StateInfo
			state, err = absent, nil
		}
		if err != nil {
			return nil, nil, err
		}
		forward := current.States[spec.Selector]
		before := !slices.Contains(current.Written, spec.Selector) && sameState(state, forward.Before)
		if !before && !matchesAfter(state, forward) {
			return nil, nil, fmt.Errorf("workspace %s changed outside the migration; restoration retains the write pause", spec.Selector)
		}
		actual[spec.Selector], files[spec.Selector] = state, file
	}
	return actual, files, nil
}

func (e Engine) reverseTransfers(ctx context.Context, scratch string, actual map[string]StateInfo, files map[string]string) error {
	indices := make(map[string]int)
	for index, spec := range e.Manifest.Workspaces {
		indices[spec.Selector] = index
	}
	staging := filepath.Join(scratch, "inverse-staging.tfstate")
	var staged []restoreInstance
	var duplicates []duplicateInstance
	backups := make(map[string]string)
	for index, transfer := range e.Manifest.Moves {
		proof := e.originalProof(transfer)
		if proof == "" {
			return fmt.Errorf("reviewed source instance %s in %s lacks its original record proof", transfer.Address, transfer.Source)
		}
		original := actual[transfer.Source].Records[transfer.Address] == proof
		destination := actual[transfer.Destination].Records[transfer.NewAddress] == proof &&
			e.originalRecordProof(transfer.Destination, transfer.NewAddress) != proof
		if original {
			if destination && (transfer.Source != transfer.Destination || transfer.Address != transfer.NewAddress) {
				duplicates = append(duplicates, duplicateInstance{workspace: transfer.Source, file: files[transfer.Destination], address: transfer.NewAddress})
			}
			continue
		}
		file, address := files[transfer.Destination], transfer.NewAddress
		if !destination {
			var err error
			file, err = e.originalSnapshot(ctx, scratch, transfer.Source, indices[transfer.Source], backups)
			if err != nil {
				return err
			}
			address = transfer.Address
		}
		stage := fmt.Sprintf("module.restore_%d.%s", index, transfer.Address)
		if err := Move(ctx, e.inverseWorkspace(transfer.Source, indices[transfer.Source]), file, staging, address, stage); err != nil {
			slog.Error("migration.restore.stage_failed", "address", transfer.Address, "err", err)
			return fmt.Errorf("stage original instance %s for restoration: %w", transfer.Address, err)
		}
		staged = append(staged, restoreInstance{transfer: transfer, staged: stage})
	}
	if err := e.removeDuplicateInstances(ctx, indices, duplicates); err != nil {
		return err
	}
	for _, instance := range staged {
		transfer := instance.transfer
		workspace := e.inverseWorkspace(transfer.Source, indices[transfer.Source])
		if err := Move(ctx, workspace, staging, files[transfer.Source], instance.staged, transfer.Address); err != nil {
			slog.Error("migration.restore.instance_failed", "address", transfer.Address, "err", err)
			return fmt.Errorf("restore original instance %s: %w", transfer.Address, err)
		}
	}
	return nil
}

func (e Engine) removeDuplicateInstances(ctx context.Context, indices map[string]int, duplicates []duplicateInstance) error {
	for _, duplicate := range duplicates {
		workspace := e.inverseWorkspace(duplicate.workspace, indices[duplicate.workspace])
		if err := requireLocalBackend(workspace); err != nil {
			return err
		}
		if err := native(ctx, workspace, io.Discard, "state", "rm", "-state="+duplicate.file, duplicate.address); err != nil {
			slog.Error("migration.restore.duplicate_removal_failed", "address", duplicate.address, "err", err)
			return fmt.Errorf("remove a verified duplicate ownership record: %w", err)
		}
		if err := protectNativeFiles(filepath.Dir(duplicate.file)); err != nil {
			return err
		}
	}
	return nil
}

func (e Engine) originalProof(transfer Transfer) string {
	return e.originalRecordProof(transfer.Source, transfer.Address)
}

func (e Engine) originalRecordProof(selector, address string) string {
	for _, spec := range e.Manifest.Workspaces {
		if spec.Selector == selector {
			return spec.Before.Records[address]
		}
	}
	return ""
}

func (e Engine) inverseWorkspace(selector string, index int) Workspace {
	return Workspace{Directory: e.snapshotDirectory("before", index), Environment: e.Workspaces[selector].Environment}
}

func (e Engine) originalSnapshot(ctx context.Context, scratch, selector string, index int, backups map[string]string) (string, error) {
	if file, exists := backups[selector]; exists {
		return file, nil
	}
	file := filepath.Join(scratch, fmt.Sprintf("original-%04d.tfstate", index))
	actual, err := Pull(ctx, e.inverseWorkspace(selector, index), file)
	if err != nil {
		return "", err
	}
	before := e.Manifest.Workspaces[index].Before
	var unconfirmed StateInfo
	if !matchesAfter(actual, checkpoint{Before: before, Prepared: before, After: unconfirmed}) {
		return "", fmt.Errorf("the encrypted original snapshot for %s changed", selector)
	}
	backups[selector] = file
	return file, nil
}

func (e Engine) captureRestorationCandidates(ctx context.Context, scratch string, actual map[string]StateInfo, files map[string]string) (map[string]checkpoint, error) {
	recovery := make(map[string]checkpoint)
	var unconfirmed StateInfo
	for index, spec := range e.Manifest.Workspaces {
		base := actual[spec.Selector]
		if base.Lineage == "" {
			recovery[spec.Selector] = checkpoint{Before: base, Prepared: base, After: unconfirmed}
			continue
		}
		candidate, err := Inspect(files[spec.Selector])
		if err != nil {
			return nil, err
		}
		if candidate.Lineage != base.Lineage || candidate.Serial < base.Serial {
			return nil, fmt.Errorf("restoration changed the native lineage or reduced the serial of %s", spec.Selector)
		}
		if err := verifyOriginalRecords(spec, candidate); err != nil {
			return nil, err
		}
		snapshot, err := Snapshot(ctx, e.Workspaces[spec.Selector], files[spec.Selector], e.snapshotDirectory("restore", index))
		if err != nil {
			return nil, err
		}
		prepared, err := Pull(ctx, snapshot, filepath.Join(scratch, fmt.Sprintf("restore-roundtrip-%04d.tfstate", index)))
		if err != nil || !samePayload(candidate, prepared) || prepared.Serial < candidate.Serial || prepared.Serial-candidate.Serial > 2 {
			return nil, fmt.Errorf("the encrypted inverse snapshot for %s changed its payload", spec.Selector)
		}
		recovery[spec.Selector] = checkpoint{Before: base, Prepared: prepared, After: unconfirmed}
	}
	return recovery, nil
}

func verifyOriginalRecords(spec WorkspaceSpec, actual StateInfo) error {
	if len(actual.Records) != len(spec.Before.Records) {
		return fmt.Errorf("restored workspace %s has an unexpected record count", spec.Selector)
	}
	for address, proof := range spec.Before.Records {
		if actual.Records[address] != proof || !validDependencyChange(spec.Before.Dependencies[address], actual.Dependencies[address]) {
			return fmt.Errorf("restored instance %s in %s differs from its original provider record", address, spec.Selector)
		}
	}
	return nil
}

func samePayload(left, right StateInfo) bool {
	return left.Lineage == right.Lineage && left.Digest == right.Digest && left.Content == right.Content
}

func (e Engine) checkRestoration(ctx context.Context, current journal, restoredOnly bool) error {
	if len(current.Recovery) != len(e.Manifest.Workspaces) {
		return errors.New("the restoration checkpoint omits required workspaces")
	}
	if err := e.verifyRestorationCandidates(ctx, current); err != nil {
		return err
	}
	for _, spec := range e.Manifest.Workspaces {
		state, exists := current.Recovery[spec.Selector]
		if !exists {
			return fmt.Errorf("the restoration checkpoint omits workspace %s", spec.Selector)
		}
		actual, err := e.currentState(ctx, spec)
		if err != nil {
			return err
		}
		if matchesAfter(actual, state) {
			continue
		}
		if !restoredOnly && !slices.Contains(current.Restored, spec.Selector) && sameState(actual, state.Before) {
			continue
		}
		return fmt.Errorf("workspace %s differs from its restoration checkpoint; the write pause remains active", spec.Selector)
	}
	return nil
}

func (e Engine) restoreWorkspace(ctx context.Context, index int, spec WorkspaceSpec, current *journal) (result error) {
	state := current.Recovery[spec.Selector]
	actual, err := e.currentState(ctx, spec)
	if err != nil {
		return err
	}
	if matchesAfter(actual, state) {
		return e.confirmRestoration(spec.Selector, actual, current)
	}
	if slices.Contains(current.Restored, spec.Selector) || !sameState(actual, state.Before) {
		return fmt.Errorf("workspace %s changed before its inverse write; the pause remains active", spec.Selector)
	}
	scratch, err := os.MkdirTemp(e.Directory, "restore-write-")
	if err != nil {
		slog.Error("migration.restore.write_directory_failed", "workspace", spec.Selector, "err", err)
		return fmt.Errorf("create a private inverse write directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			result = errors.Join(result, fmt.Errorf("remove transient decrypted inverse write files: %w", err))
		}
	}()
	file := filepath.Join(scratch, "restore.tfstate")
	snapshot := Workspace{Directory: e.snapshotDirectory("restore", index), Environment: e.Workspaces[spec.Selector].Environment}
	prepared, err := Pull(ctx, snapshot, file)
	if err != nil || !sameState(prepared, state.Prepared) {
		return fmt.Errorf("the encrypted restoration candidate for %s failed verification", spec.Selector)
	}
	if samePayload(actual, prepared) {
		return e.confirmRestoration(spec.Selector, actual, current)
	}
	actual, err = e.pushRestorationCandidate(ctx, spec, state, file)
	if err != nil {
		return err
	}
	return e.confirmRestoration(spec.Selector, actual, current)
}

func (e Engine) pushRestorationCandidate(ctx context.Context, spec WorkspaceSpec, state checkpoint, file string) (StateInfo, error) {
	if err := e.requirePause(); err != nil {
		return StateInfo{}, err
	}
	base, err := e.currentState(ctx, spec)
	if err != nil || !sameState(base, state.Before) {
		return StateInfo{}, fmt.Errorf("workspace %s changed immediately before its inverse write", spec.Selector)
	}
	if err := Push(ctx, e.Workspaces[spec.Selector], file); err != nil {
		slog.Error("migration.restore.push_failed", "workspace", spec.Selector, "err", err)
		return StateInfo{}, fmt.Errorf("restore workspace %s; inspect its actual state before retrying: %w", spec.Selector, err)
	}
	actual, err := e.currentState(ctx, spec)
	if err != nil || !matchesAfter(actual, state) {
		return StateInfo{}, fmt.Errorf("workspace %s did not confirm its inverse write; the pause remains active", spec.Selector)
	}
	return actual, nil
}

func (e Engine) confirmRestoration(selector string, actual StateInfo, current *journal) error {
	state := current.Recovery[selector]
	state.After = actual
	current.Recovery[selector] = state
	if !slices.Contains(current.Restored, selector) {
		current.Restored = append(current.Restored, selector)
	}
	return e.saveJournal(*current)
}

// VerifyRestored requires the original configuration and zero-change source plans.
// It releases the pause only after all restored records and plans verify.
func (e Engine) VerifyRestored(ctx context.Context) error {
	release, err := e.acquire()
	if err != nil {
		return err
	}
	defer release()
	releasePause, err := e.acquirePauseLease()
	if err != nil {
		return err
	}
	defer releasePause()
	if e.CurrentCommit != e.Manifest.RestoreCommit {
		return errors.New("restore verification requires the reviewed original configuration commit")
	}
	current, err := e.loadJournal()
	if err != nil {
		return err
	}
	if current.Phase != phaseRestored && current.Phase != phaseRestoredVerified {
		return fmt.Errorf("cannot verify restoration in phase %s", current.Phase)
	}
	if err := e.requirePause(); err != nil {
		return err
	}
	if err := e.removeRestorationScratch(); err != nil {
		return err
	}
	if err := e.checkRestoration(ctx, current, true); err != nil {
		return err
	}
	if err := e.verifyRestoredRoots(ctx); err != nil {
		return err
	}
	if err := e.checkRestoration(ctx, current, true); err != nil {
		return err
	}
	current.Phase = phaseRestoredVerified
	if err := e.saveJournal(current); err != nil {
		return err
	}
	if err := os.Remove(e.PauseFile); err != nil {
		slog.Error("migration.restore.pause_release_failed", "err", err)
		return fmt.Errorf("release the verified restoration pause: %w", err)
	}
	return syncDirectory(filepath.Dir(e.PauseFile))
}

func (e Engine) verifyRestoredRoots(ctx context.Context) error {
	for _, spec := range e.Manifest.Workspaces {
		actual, err := e.currentState(ctx, spec)
		if err != nil {
			return err
		}
		if err := verifyOriginalRecords(spec, actual); err != nil {
			return err
		}
		if spec.Before.Lineage == "" {
			continue
		}
		workspace, exists := e.RestoreWorkspaces[spec.Selector]
		if !exists || workspace.Directory == "" {
			return fmt.Errorf("restored workspace %s lacks its reviewed original configuration for plan verification", spec.Selector)
		}
		if err := CheckPlan(ctx, workspace, e.Directory); err != nil {
			slog.Error("migration.restore.original_plan_failed", "workspace", spec.Selector, "err", err)
			return fmt.Errorf("restored workspace %s must pass its original plan before releasing the pause: %w", spec.Selector, err)
		}
	}
	return nil
}

func (e Engine) retainOrphanedCandidates() error {
	for index := range e.Manifest.Workspaces {
		path := e.snapshotDirectory("restore", index)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			slog.Error("migration.restore.orphan_inspection_failed", "err", err)
			return fmt.Errorf("inspect an uncheckpointed inverse candidate: %w", err)
		}
		orphan, err := os.MkdirTemp(e.Directory, "restore-orphan-")
		if err != nil {
			slog.Error("migration.restore.orphan_directory_failed", "err", err)
			return fmt.Errorf("retain an uncheckpointed encrypted candidate: %w", err)
		}
		if err := os.Rename(path, filepath.Join(orphan, "candidate")); err != nil {
			slog.Error("migration.restore.orphan_retention_failed", "err", err)
			return fmt.Errorf("preserve an uncheckpointed encrypted inverse candidate: %w", err)
		}
		if err := syncDirectory(orphan); err != nil {
			return err
		}
	}
	return syncDirectory(e.Directory)
}

func (e Engine) removeRestorationScratch() error {
	entries, err := os.ReadDir(e.Directory)
	if err != nil {
		slog.Error("migration.restore.scratch_inspection_failed", "err", err)
		return fmt.Errorf("inspect private transient restoration files: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "restore-prepare-") && !strings.HasPrefix(entry.Name(), "restore-write-") && !strings.HasPrefix(entry.Name(), "restore-check-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(e.Directory, entry.Name())); err != nil {
			slog.Error("migration.restore.scratch_removal_failed", "err", err)
			return fmt.Errorf("remove abandoned decrypted restoration files: %w", err)
		}
	}
	return nil
}

func (e Engine) verifyRestorationCandidates(ctx context.Context, current journal) (result error) {
	scratch, err := os.MkdirTemp(e.Directory, "restore-check-")
	if err != nil {
		slog.Error("migration.restore.verification_directory_failed", "err", err)
		return fmt.Errorf("create a private inverse candidate verification directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			result = errors.Join(result, fmt.Errorf("remove decrypted inverse candidate verification files: %w", err))
		}
	}()
	for index, spec := range e.Manifest.Workspaces {
		state, exists := current.Recovery[spec.Selector]
		if !exists {
			return fmt.Errorf("the inverse checkpoint omits workspace %s", spec.Selector)
		}
		if state.Prepared.Lineage == "" {
			if spec.Before.Lineage != "" || state.Before.Lineage != "" {
				return fmt.Errorf("the inverse checkpoint omits an existing state for %s", spec.Selector)
			}
			continue
		}
		snapshot := Workspace{Directory: e.snapshotDirectory("restore", index), Environment: e.Workspaces[spec.Selector].Environment}
		candidate, err := Pull(ctx, snapshot, filepath.Join(scratch, stateName(index)))
		if err != nil || !sameState(candidate, state.Prepared) {
			return fmt.Errorf("the recorded inverse candidate for %s changed", spec.Selector)
		}
		if err := verifyOriginalRecords(spec, candidate); err != nil {
			return err
		}
	}
	return nil
}
