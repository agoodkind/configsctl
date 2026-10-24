package statemigration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
)

// Engine coordinates a complete transfer while ordinary writers remain paused.
type Engine struct {
	Manifest          Manifest
	CurrentCommit     string
	Directory         string
	PauseFile         string
	Workspaces        map[string]Workspace
	RestoreWorkspaces map[string]Workspace
}

type checkpoint struct {
	Before   StateInfo `json:"before"`
	Prepared StateInfo `json:"prepared"`
	After    StateInfo `json:"after"`
}

type journal struct {
	Manifest string                `json:"manifest"`
	Phase    string                `json:"phase"`
	States   map[string]checkpoint `json:"states"`
	Written  []string              `json:"written"`
	Restored []string              `json:"restored"`
	Recovery map[string]checkpoint `json:"recovery"`
}

const (
	phasePrepared    = "prepared"
	phaseApplying    = "applying"
	phaseTransferred = "transferred"
	phaseRestoring   = "restoring"
	phaseRestored    = "restored"
	phaseVerified    = "verified"
)

// Prepare verifies the full mapping and retains encrypted before and after states.
// It does not write any deployment backend or pause ordinary deployment.
func (e Engine) Prepare(ctx context.Context) (result error) {
	slog.InfoContext(ctx, "migration.prepare.started", "path", e.Directory)
	if e.CurrentCommit != e.Manifest.Commit {
		return errors.New("preparation requires the reviewed cutover configuration commit")
	}
	release, err := e.acquire()
	if err != nil {
		return err
	}
	defer release()
	if _, err := os.Lstat(filepath.Join(e.Directory, "journal.json")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("the migration directory already contains a journal; use its recovery action or a new directory")
	}
	scratch, err := os.MkdirTemp(e.Directory, "prepare-")
	if err != nil {
		slog.ErrorContext(ctx, "migration.prepare.directory_failed", "path", e.Directory, "err", migrationErrorType(err))
		return fmt.Errorf("create private preparation directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			slog.ErrorContext(ctx, "migration.prepare.cleanup_failed", "path", e.Directory, "err", migrationErrorType(err))
			result = errors.Join(result, fmt.Errorf("remove transient decrypted preparation files: %w", err))
		}
	}()
	fingerprint, err := e.Manifest.fingerprint()
	if err != nil {
		return err
	}
	prepared := journal{
		Manifest: fingerprint, Phase: phasePrepared,
		States: make(map[string]checkpoint), Written: nil, Restored: nil, Recovery: nil,
	}
	if err := e.captureBefore(ctx, scratch, &prepared); err != nil {
		return err
	}
	if err := e.prepareTransfers(ctx, scratch, &prepared); err != nil {
		return err
	}
	if err := e.saveJournal(prepared); err != nil {
		return err
	}
	return nil
}

func (e Engine) captureBefore(ctx context.Context, scratch string, prepared *journal) error {
	for index, spec := range e.Manifest.Workspaces {
		if err := Initialize(ctx, e.Workspaces[spec.Selector]); err != nil {
			slog.ErrorContext(ctx, "migration.prepare.initialize_failed", "workspace", spec.Selector, "err", migrationErrorType(err))
			return fmt.Errorf("initialize reviewed workspace %s: %w", spec.Selector, err)
		}
		file := filepath.Join(scratch, stateName(index))
		actual, err := Pull(ctx, e.Workspaces[spec.Selector], file)
		if errors.Is(err, ErrAbsent) && spec.Before.Lineage == "" {
			prepared.States[spec.Selector] = checkpoint{Before: EmptyState(), Prepared: EmptyState(), After: EmptyState()}
			continue
		}
		if err != nil {
			slog.ErrorContext(ctx, "migration.prepare.capture_failed", "workspace", spec.Selector, "err", migrationErrorType(err))
			return fmt.Errorf("capture source workspace %s: %w", spec.Selector, err)
		}
		if !sameState(actual, spec.Before) {
			return fmt.Errorf("workspace %s changed from the reviewed source state", spec.Selector)
		}
		if _, err := Snapshot(ctx, e.Workspaces[spec.Selector], file, e.snapshotDirectory("before", index)); err != nil {
			slog.ErrorContext(ctx, "migration.prepare.before_snapshot_failed", "workspace", spec.Selector, "err", migrationErrorType(err))
			return fmt.Errorf("encrypt workspace %s before migration: %w", spec.Selector, err)
		}
		prepared.States[spec.Selector] = checkpoint{Before: actual, Prepared: EmptyState(), After: EmptyState()}
	}
	return nil
}

func (e Engine) prepareTransfers(ctx context.Context, scratch string, prepared *journal) error {
	indices := make(map[string]int)
	for index, spec := range e.Manifest.Workspaces {
		indices[spec.Selector] = index
	}
	staged := filepath.Join(scratch, "staging.tfstate")
	for index, move := range e.Manifest.Moves {
		if move.Source == move.Destination && move.Address == move.NewAddress {
			continue
		}
		source := filepath.Join(scratch, stateName(indices[move.Source]))
		address := fmt.Sprintf("module.transfer_%d.%s", index, move.Address)
		local := Workspace{Directory: e.snapshotDirectory("before", indices[move.Source]), Environment: e.Workspaces[move.Source].Environment}
		if err := Move(ctx, local, source, staged, move.Address, address); err != nil {
			slog.ErrorContext(ctx, "migration.prepare.stage_failed", "workspace", move.Source, "err", migrationErrorType(err))
			return fmt.Errorf("stage reviewed instance %s: %w", move.Address, err)
		}
	}
	for index, move := range e.Manifest.Moves {
		if move.Source == move.Destination && move.Address == move.NewAddress {
			continue
		}
		destination := filepath.Join(scratch, stateName(indices[move.Destination]))
		address := fmt.Sprintf("module.transfer_%d.%s", index, move.Address)
		local := Workspace{Directory: e.snapshotDirectory("before", indices[move.Source]), Environment: e.Workspaces[move.Source].Environment}
		if err := Move(ctx, local, staged, destination, address, move.NewAddress); err != nil {
			slog.ErrorContext(ctx, "migration.prepare.destination_failed", "workspace", move.Destination, "err", migrationErrorType(err))
			return fmt.Errorf("prepare reviewed destination %s: %w", move.NewAddress, err)
		}
	}
	return e.captureAfter(ctx, scratch, prepared)
}

func (e Engine) captureAfter(ctx context.Context, scratch string, prepared *journal) error {
	for index, spec := range e.Manifest.Workspaces {
		file := filepath.Join(scratch, stateName(index))
		after, err := Inspect(file)
		if err != nil {
			slog.ErrorContext(ctx, "migration.prepare.inspect_failed", "workspace", spec.Selector, "err", migrationErrorType(err))
			return fmt.Errorf("inspect prepared workspace %s: %w", spec.Selector, err)
		}
		if err := e.verifyAssignments(spec.Selector, after); err != nil {
			return err
		}
		snapshot, err := Snapshot(ctx, e.Workspaces[spec.Selector], file, e.snapshotDirectory("after", index))
		if err != nil {
			slog.ErrorContext(ctx, "migration.prepare.after_snapshot_failed", "workspace", spec.Selector, "err", migrationErrorType(err))
			return fmt.Errorf("encrypt prepared workspace %s: %w", spec.Selector, err)
		}
		roundTrip := filepath.Join(scratch, fmt.Sprintf("verified-%04d.tfstate", index))
		snapshotState, err := Pull(ctx, snapshot, roundTrip)
		if err != nil || snapshotState.Lineage != after.Lineage || snapshotState.Digest != after.Digest || snapshotState.Content != after.Content {
			return fmt.Errorf("the native encrypted snapshot for %s changed resource metadata", spec.Selector)
		}
		state := prepared.States[spec.Selector]
		state.Prepared = snapshotState
		prepared.States[spec.Selector] = state
	}
	return nil
}

func (e Engine) verifyAssignments(selector string, actual StateInfo) error {
	expected := make(map[string]string)
	for _, move := range e.Manifest.Moves {
		if move.Destination != selector {
			continue
		}
		for _, source := range e.Manifest.Workspaces {
			if source.Selector == move.Source {
				expected[move.NewAddress] = source.Before.Records[move.Address]
				break
			}
		}
	}
	if len(expected) != len(actual.Records) {
		return fmt.Errorf("prepared workspace %s contains an unexpected instance count", selector)
	}
	for address, proof := range expected {
		if proof == "" || actual.Records[address] != proof {
			return fmt.Errorf("prepared instance %s in %s changed identity, attributes, or provider state", address, selector)
		}
	}
	for _, move := range e.Manifest.Moves {
		if move.Destination != selector {
			continue
		}
		for _, source := range e.Manifest.Workspaces {
			if source.Selector == move.Source && !validDependencyChange(source.Before.Dependencies[move.Address], actual.Dependencies[move.NewAddress]) {
				return fmt.Errorf("prepared instance %s contains an unexpected dependency change", move.NewAddress)
			}
		}
	}
	return nil
}

func validDependencyChange(before, after []string) bool {
	return len(after) == 0 || slices.Equal(before, after)
}

// Execute resumes only states that match the verified before or after snapshots.
// It retains the durable pause until Verify proves the entire transfer.
func (e Engine) Execute(ctx context.Context) error {
	if e.CurrentCommit != e.Manifest.Commit {
		return errors.New("execution requires the reviewed cutover configuration commit")
	}
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
	if current.Phase != phasePrepared && current.Phase != phaseApplying && current.Phase != phaseTransferred {
		return fmt.Errorf("cannot execute a migration in phase %s", current.Phase)
	}
	if err := e.pause(); err != nil {
		return err
	}
	if err := e.checkSet(ctx, current, false); err != nil {
		return err
	}
	current.Phase = phaseApplying
	if err := e.saveJournal(current); err != nil {
		return err
	}
	for index, spec := range e.Manifest.Workspaces {
		if err := e.writeOne(ctx, index, spec, &current); err != nil {
			return err
		}
	}
	current.Phase = phaseTransferred
	return e.saveJournal(current)
}

func (e Engine) writeOne(ctx context.Context, index int, spec WorkspaceSpec, current *journal) (result error) {
	slog.InfoContext(ctx, "migration.workspace_write.started", "workspace", spec.Selector)
	state := current.States[spec.Selector]
	actual, err := e.currentState(ctx, spec)
	if err != nil {
		return err
	}
	if matchesAfter(actual, state) {
		if !slices.Contains(current.Written, spec.Selector) {
			current.Written = append(current.Written, spec.Selector)
		}
		state.After = actual
		current.States[spec.Selector] = state
		return e.saveJournal(*current)
	}
	if slices.Contains(current.Written, spec.Selector) || !sameState(actual, state.Before) {
		return fmt.Errorf("workspace %s changed outside this migration; the write pause remains active", spec.Selector)
	}
	scratch, err := os.MkdirTemp(e.Directory, "write-")
	if err != nil {
		slog.ErrorContext(ctx, "migration.workspace_write.directory_failed", "workspace", spec.Selector, "err", migrationErrorType(err))
		return fmt.Errorf("create private backend write directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			slog.ErrorContext(ctx, "migration.workspace_write.cleanup_failed", "workspace", spec.Selector, "err", migrationErrorType(err))
			result = errors.Join(result, fmt.Errorf("remove transient decrypted backend write files: %w", err))
		}
	}()
	file := filepath.Join(scratch, "prepared.tfstate")
	snapshot := Workspace{Directory: e.snapshotDirectory("after", index), Environment: e.Workspaces[spec.Selector].Environment}
	prepared, err := Pull(ctx, snapshot, file)
	if err != nil || !sameState(prepared, state.Prepared) {
		return fmt.Errorf("the encrypted prepared snapshot for %s failed verification", spec.Selector)
	}
	if err := e.requirePause(); err != nil {
		return err
	}
	if err := Push(ctx, e.Workspaces[spec.Selector], file); err != nil {
		slog.ErrorContext(ctx, "migration.workspace_write.push_failed", "workspace", spec.Selector, "err", migrationErrorType(err))
		return fmt.Errorf("write workspace %s; inspect actual state with resume before retrying: %w", spec.Selector, err)
	}
	actual, err = e.currentState(ctx, spec)
	if err != nil || !matchesAfter(actual, state) {
		return fmt.Errorf("workspace %s did not confirm the prepared state; the write pause remains active", spec.Selector)
	}
	state.After = actual
	current.States[spec.Selector] = state
	current.Written = append(current.Written, spec.Selector)
	if err := e.saveJournal(*current); err != nil {
		return err
	}
	return nil
}

func (e Engine) currentState(ctx context.Context, spec WorkspaceSpec) (StateInfo, error) {
	slog.InfoContext(ctx, "migration.workspace_inspection.started", "workspace", spec.Selector)
	scratch, err := os.MkdirTemp(e.Directory, "inspect-")
	if err != nil {
		slog.ErrorContext(ctx, "migration.workspace_inspection.directory_failed", "workspace", spec.Selector, "err", migrationErrorType(err))
		return EmptyState(), fmt.Errorf("create private verification directory: %w", err)
	}
	actual, readErr := Pull(ctx, e.Workspaces[spec.Selector], filepath.Join(scratch, "actual.tfstate"))
	if errors.Is(readErr, ErrAbsent) {
		actual, readErr = EmptyState(), nil
	}
	if err := os.RemoveAll(scratch); err != nil {
		slog.ErrorContext(ctx, "migration.workspace_inspection.cleanup_failed", "workspace", spec.Selector, "err", migrationErrorType(err))
		return EmptyState(), fmt.Errorf("remove transient state verification files: %w", err)
	}
	return actual, readErr
}

func (e Engine) checkSet(ctx context.Context, current journal, afterOnly bool) error {
	for _, spec := range e.Manifest.Workspaces {
		actual, err := e.currentState(ctx, spec)
		if err != nil {
			return err
		}
		state := current.States[spec.Selector]
		if matchesAfter(actual, state) {
			continue
		}
		if !afterOnly && !slices.Contains(current.Written, spec.Selector) && sameState(actual, state.Before) {
			continue
		}
		return fmt.Errorf("workspace %s differs from its migration checkpoint; the write pause remains active", spec.Selector)
	}
	return nil
}

// Verify checks every final state and requires independent zero-change native plans.
// It releases the pause only after every workspace passes both checks.
func (e Engine) Verify(ctx context.Context) error {
	slog.InfoContext(ctx, "migration.verify.started", "path", e.Directory)
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
	if current.Phase != phaseTransferred && current.Phase != phaseVerified {
		return fmt.Errorf("cannot verify a migration in phase %s", current.Phase)
	}
	if err := e.requirePause(); err != nil {
		return err
	}
	if err := e.checkSet(ctx, current, true); err != nil {
		return err
	}
	for _, spec := range e.Manifest.Workspaces {
		if err := CheckPlan(ctx, e.Workspaces[spec.Selector], e.Directory); err != nil {
			slog.ErrorContext(ctx, "migration.verify.plan_failed", "workspace", spec.Selector, "err", migrationErrorType(err))
			return fmt.Errorf("workspace %s must pass its independent plan before releasing the pause: %w", spec.Selector, err)
		}
	}
	if err := e.checkSet(ctx, current, true); err != nil {
		return err
	}
	current.Phase = phaseVerified
	if err := e.saveJournal(current); err != nil {
		return err
	}
	if err := os.Remove(e.PauseFile); err != nil {
		slog.ErrorContext(ctx, "migration.verify.pause_release_failed", "path", e.PauseFile, "err", migrationErrorType(err))
		return fmt.Errorf("release the verified migration pause: %w", err)
	}
	return syncDirectory(filepath.Dir(e.PauseFile))
}

func stateName(index int) string { return fmt.Sprintf("workspace-%04d.tfstate", index) }

func (e Engine) snapshotDirectory(stage string, index int) string {
	return filepath.Join(e.Directory, fmt.Sprintf("%s-%04d", stage, index))
}

func sameState(left, right StateInfo) bool {
	if left.Lineage == "" || right.Lineage == "" {
		return left.Lineage == right.Lineage && left.Serial == right.Serial && len(left.Records) == 0 && len(right.Records) == 0
	}
	return left.Lineage == right.Lineage && left.Serial == right.Serial && left.Digest == right.Digest && left.Content == right.Content
}

func matchesAfter(actual StateInfo, state checkpoint) bool {
	if state.After.Lineage != "" {
		return sameState(actual, state.After)
	}
	prepared := state.Prepared
	if prepared.Lineage == "" {
		return absentStateMetadata(actual) && absentStateMetadata(state.Before) &&
			absentStateMetadata(prepared) && absentStateMetadata(state.After)
	}
	if actual.Lineage == "" || actual.Digest != prepared.Digest || actual.Content != prepared.Content {
		return false
	}
	// An absent remote backend creates its own lineage at serial 1 on first persistence.
	if state.Before.Lineage == "" && actual.Serial == 1 {
		return true
	}
	return actual.Lineage == prepared.Lineage &&
		actual.Serial >= state.Before.Serial && actual.Serial >= prepared.Serial &&
		actual.Serial-prepared.Serial <= 2
}

func absentStateMetadata(state StateInfo) bool {
	return state.Lineage == "" && state.Serial == 0 && state.Digest == "" && state.Content == "" &&
		len(state.Records) == 0 && len(state.Dependencies) == 0
}

func (e Engine) acquire() (func(), error) {
	if err := e.Manifest.Validate(); err != nil {
		return nil, err
	}
	if e.Manifest.Commit != e.CurrentCommit && e.Manifest.RestoreCommit != e.CurrentCommit {
		return nil, errors.New("the current configuration commit differs from both reviewed migration commits")
	}
	for _, spec := range e.Manifest.Workspaces {
		workspace, exists := e.Workspaces[spec.Selector]
		if !exists || workspace.Directory == "" {
			return nil, fmt.Errorf("workspace %s lacks its initialized native backend", spec.Selector)
		}
	}
	root, err := privateDirectory(e.Directory)
	if err != nil {
		return nil, err
	}
	file, err := root.OpenFile("executor.lock", os.O_CREATE|os.O_RDWR, privateFileMode)
	if err != nil {
		_ = root.Close()
		slog.Error("migration.executor_lock.open_failed", "path", e.Directory, "err", migrationErrorType(err))
		return nil, fmt.Errorf("open migration executor lock: %w", err)
	}
	if err := lockExecutor(file); err != nil {
		_ = file.Close()
		_ = root.Close()
		return nil, err
	}
	if err := cleanupInterruptedFiles(root, e.Directory); err != nil {
		_ = file.Close()
		_ = root.Close()
		return nil, err
	}
	return func() { _ = file.Close(); _ = root.Close() }, nil
}

func (e Engine) saveJournal(current journal) error {
	slog.Info("migration.journal.save_started", "path", e.Directory)
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		slog.Error("migration.journal.encode_failed", "path", e.Directory, "err", migrationErrorType(err))
		return fmt.Errorf("encode migration checkpoint: %w", err)
	}
	file, err := os.CreateTemp(e.Directory, "journal-next-")
	if err != nil {
		slog.Error("migration.journal.create_failed", "path", e.Directory, "err", migrationErrorType(err))
		return fmt.Errorf("create migration checkpoint: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		slog.Error("migration.journal.write_failed", "path", e.Directory, "err", migrationErrorType(err))
		return fmt.Errorf("write migration checkpoint: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		slog.Error("migration.journal.sync_failed", "path", e.Directory, "err", migrationErrorType(err))
		return fmt.Errorf("sync migration checkpoint: %w", err)
	}
	if err := file.Close(); err != nil {
		slog.Error("migration.journal.close_failed", "path", e.Directory, "err", migrationErrorType(err))
		return fmt.Errorf("close migration checkpoint: %w", err)
	}
	if err := os.Rename(file.Name(), filepath.Join(e.Directory, "journal.json")); err != nil {
		slog.Error("migration.journal.replace_failed", "path", e.Directory, "err", migrationErrorType(err))
		return fmt.Errorf("replace migration checkpoint: %w", err)
	}
	directory, err := os.Open(e.Directory)
	if err != nil {
		slog.Error("migration.journal.directory_open_failed", "path", e.Directory, "err", migrationErrorType(err))
		return fmt.Errorf("open migration directory for checkpoint sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		slog.Error("migration.journal.directory_sync_failed", "path", e.Directory, "err", migrationErrorType(err))
		return fmt.Errorf("sync migration checkpoint directory: %w", err)
	}
	return nil
}

func (e Engine) loadJournal() (journal, error) {
	data, err := os.ReadFile(filepath.Join(e.Directory, "journal.json"))
	if err != nil {
		slog.Error("migration.journal.read_failed", "path", e.Directory, "err", migrationErrorType(err))
		return journal{}, fmt.Errorf("read migration checkpoint: %w", err)
	}
	fingerprint, err := e.Manifest.fingerprint()
	if err != nil {
		return journal{}, err
	}
	var current journal
	if err := json.Unmarshal(data, &current); err != nil || current.Manifest != fingerprint {
		return journal{}, errors.New("the migration checkpoint does not match the reviewed manifest")
	}
	if len(current.States) != len(e.Manifest.Workspaces) {
		return journal{}, errors.New("the migration checkpoint omits required workspaces")
	}
	for _, spec := range e.Manifest.Workspaces {
		if _, exists := current.States[spec.Selector]; !exists {
			return journal{}, fmt.Errorf("the migration checkpoint omits workspace %s", spec.Selector)
		}
	}
	return current, nil
}
