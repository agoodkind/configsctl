package statemigration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"

	"goodkind.io/configsctl/internal/migrationpause"
	"goodkind.io/configsctl/internal/statemigration"
)

func restorationFixture(t *testing.T) (statemigration.Engine, statemigration.StateInfo) {
	t.Helper()
	engine, before := migrationFixture(t)
	if err := engine.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	return engine, before
}

func writePreparedState(t *testing.T, engine statemigration.Engine, index int) {
	t.Helper()
	spec := engine.Manifest.Workspaces[index]
	snapshot := statemigration.Workspace{
		Directory:   filepath.Join(engine.Directory, fmt.Sprintf("after-%04d", index)),
		Environment: engine.Workspaces[spec.Selector].Environment,
	}
	file := filepath.Join(privateFixtureDir(t), "acknowledged.tfstate")
	if _, err := statemigration.Pull(t.Context(), snapshot, file); err != nil {
		t.Fatal(err)
	}
	if err := statemigration.Push(t.Context(), engine.Workspaces[spec.Selector], file); err != nil {
		t.Fatal(err)
	}
}

func restoredState(t *testing.T, engine statemigration.Engine, selector string) statemigration.StateInfo {
	t.Helper()
	file := filepath.Join(privateFixtureDir(t), "actual.tfstate")
	actual, err := statemigration.Pull(t.Context(), engine.Workspaces[selector], file)
	if errors.Is(err, statemigration.ErrAbsent) {
		return statemigration.StateInfo{}
	}
	if err != nil {
		t.Fatal(err)
	}
	return actual
}

func requireOriginalState(t *testing.T, engine statemigration.Engine, before statemigration.StateInfo) {
	t.Helper()
	actual := restoredState(t, engine, "legacy")
	if actual.Lineage != before.Lineage || actual.Serial < before.Serial || !reflect.DeepEqual(actual.Records, before.Records) {
		t.Fatal("restoration changed the original lineage, reduced its serial, or changed a complete provider record")
	}
	destination := restoredState(t, engine, "application")
	if len(destination.Records) != 0 {
		t.Fatal("restoration retained transferred ownership records in the destination")
	}
	if err := migrationpause.Guard(engine.PauseFile); err == nil {
		t.Fatal("restoration released the write pause before original plans verified")
	}
}

func requirePrivateRecoveryFiles(t *testing.T, engine statemigration.Engine) {
	t.Helper()
	if err := filepath.WalkDir(engine.Directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), "restore-prepare-") || strings.HasPrefix(entry.Name(), "restore-write-") || strings.HasPrefix(entry.Name(), "restore-check-") {
				t.Fatal("restoration retained a transient decrypted directory")
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(syntheticMarker)) {
			t.Fatal("restoration retained the synthetic state value in plaintext")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRecoversAfterAnUnconfirmedSourceWrite(t *testing.T) {
	engine, before := restorationFixture(t)
	writePreparedState(t, engine, 0)
	empty := restoredState(t, engine, "legacy")
	if len(empty.Records) != 0 {
		t.Fatal("the real partial forward write did not remove the original record")
	}
	if err := engine.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	requireOriginalState(t, engine, before)
	if restoredState(t, engine, "legacy").Serial <= empty.Serial {
		t.Fatal("the inverse native operations did not advance the actual source serial")
	}
	if err := engine.VerifyRestored(t.Context()); err == nil {
		t.Fatal("restoration verification accepted the cutover configuration revision")
	}
	engine.CurrentCommit = engine.Manifest.RestoreCommit
	if err := engine.VerifyRestored(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := migrationpause.Guard(engine.PauseFile); err != nil {
		t.Fatal("verified restoration retained the infrastructure pause")
	}
	requirePrivateRecoveryFiles(t, engine)
}

func TestRestoreRemovesOnlyTheConfirmedDuplicateRecord(t *testing.T) {
	engine, before := restorationFixture(t)
	writePreparedState(t, engine, 1)
	if err := engine.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	requireOriginalState(t, engine, before)
	if restoredState(t, engine, "legacy").Serial != before.Serial {
		t.Fatal("duplicate cleanup rewrote the original source state")
	}
	requirePrivateRecoveryFiles(t, engine)
}

func TestRestoreReversesTheCompleteUnverifiedTransfer(t *testing.T) {
	engine, before := restorationFixture(t)
	if err := engine.Execute(t.Context()); err != nil {
		t.Fatal(err)
	}
	forward := restoredState(t, engine, "legacy")
	if err := engine.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	requireOriginalState(t, engine, before)
	if restoredState(t, engine, "legacy").Serial <= forward.Serial {
		t.Fatal("complete restoration did not preserve and advance the current source serial")
	}
	stale := filepath.Join(engine.Directory, "restore-check-interrupted")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := statemigration.Pull(t.Context(), engine.Workspaces["legacy"], filepath.Join(stale, "actual.tfstate")); err != nil {
		t.Fatal(err)
	}
	engine.CurrentCommit = engine.Manifest.RestoreCommit
	workspace := engine.RestoreWorkspaces["legacy"]
	originalEnvironment := workspace.Environment
	workspace.Environment = append(slices.Clone(originalEnvironment), "TF_VAR_fixture_value=synthetic-unapplied-original-change")
	engine.RestoreWorkspaces["legacy"] = workspace
	if err := engine.VerifyRestored(t.Context()); err == nil {
		t.Fatal("restoration verification ignored a changed original declaration")
	}
	if err := migrationpause.Guard(engine.PauseFile); err == nil {
		t.Fatal("a failed original plan released the write pause")
	}
	workspace.Environment = originalEnvironment
	engine.RestoreWorkspaces["legacy"] = workspace
	if err := engine.VerifyRestored(t.Context()); err != nil {
		t.Fatal(err)
	}
	requirePrivateRecoveryFiles(t, engine)
}

func TestRestoreRefusesAnUnrelatedStateUpdate(t *testing.T) {
	engine, _ := restorationFixture(t)
	if err := engine.Execute(t.Context()); err != nil {
		t.Fatal(err)
	}
	workspace := engine.Workspaces["application"]
	workspace.Environment = append(slices.Clone(workspace.Environment), "TF_VAR_fixture_value=synthetic-unrelated-destination-update")
	nativeCommand(t, workspace, "apply", "-auto-approve", "-input=false", "-no-color")
	before := restoredState(t, engine, "application")
	err := engine.Restore(t.Context())
	if err == nil {
		t.Fatal("restoration overwrote an unrelated destination update")
	}
	if strings.Contains(err.Error(), syntheticMarker) {
		t.Fatal("a refused restoration exposed a state value")
	}
	after := restoredState(t, engine, "application")
	if before.Lineage != after.Lineage || before.Serial != after.Serial || before.Content != after.Content {
		t.Fatal("refused restoration changed the unrelated destination state")
	}
	if err := migrationpause.Guard(engine.PauseFile); err == nil {
		t.Fatal("refused restoration removed the durable write pause")
	}
	requirePrivateRecoveryFiles(t, engine)
}

func TestRestoreResumesAnAcknowledgedInverseWrite(t *testing.T) {
	engine, before := restorationFixture(t)
	if err := engine.Execute(t.Context()); err != nil {
		t.Fatal(err)
	}
	checkpoint := captureInverseCheckpoint(t, engine)
	requireOriginalState(t, engine, before)
	confirmed := restoredState(t, engine, "legacy")
	if err := os.WriteFile(filepath.Join(engine.Directory, "journal.json"), checkpoint, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := engine.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := restoredState(t, engine, "legacy")
	if confirmed.Lineage != after.Lineage || confirmed.Serial != after.Serial || confirmed.Content != after.Content {
		t.Fatal("restoration repeated an already acknowledged inverse write")
	}
	engine.CurrentCommit = engine.Manifest.RestoreCommit
	if err := engine.VerifyRestored(t.Context()); err != nil {
		t.Fatal(err)
	}
	requirePrivateRecoveryFiles(t, engine)
}

func captureInverseCheckpoint(t *testing.T, engine statemigration.Engine) []byte {
	t.Helper()
	finished := make(chan error, 1)
	go func() {
		defer func() {
			if failure := recover(); failure != nil {
				finished <- fmt.Errorf("restore worker panicked: %v", failure)
			}
		}()
		finished <- engine.Restore(t.Context())
	}()
	var captured []byte
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(filepath.Join(engine.Directory, "journal.json"))
		if err != nil {
			t.Fatal(err)
		}
		var checkpoint struct {
			Phase    string                     `json:"phase"`
			Restored []string                   `json:"restored"`
			Recovery map[string]json.RawMessage `json:"recovery"`
		}
		if err := json.Unmarshal(data, &checkpoint); err != nil {
			t.Fatal(err)
		}
		if captured == nil && checkpoint.Phase == "restoring" && len(checkpoint.Recovery) == len(engine.Manifest.Workspaces) && len(checkpoint.Restored) == 0 {
			captured = data
		}
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
			if len(captured) == 0 {
				t.Fatal("the real inverse write did not expose its durable pre-write checkpoint")
			}
			return captured
		case <-timer.C:
			t.Fatal("the real inverse write did not complete before its recovery timeout")
		case <-ticker.C:
		}
	}
}

func TestRestoreRetainsUncheckpointedCandidatesAfterCancellation(t *testing.T) {
	engine, before := restorationFixture(t)
	if err := engine.Execute(t.Context()); err != nil {
		t.Fatal(err)
	}
	base := restoredState(t, engine, "legacy")
	interrupted := cancelAfterInverseSnapshot(t, engine)
	afterCancellation := restoredState(t, engine, "legacy")
	if base.Lineage != afterCancellation.Lineage || base.Serial != afterCancellation.Serial || base.Content != afterCancellation.Content {
		t.Fatal("canceled inverse preparation wrote a deployment backend")
	}
	stale := filepath.Join(engine.Directory, "restore-write-crash")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := statemigration.Pull(t.Context(), engine.Workspaces["application"], filepath.Join(stale, "actual.tfstate")); err != nil {
		t.Fatal(err)
	}
	if err := engine.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	requireOriginalState(t, engine, before)
	paths, err := filepath.Glob(filepath.Join(engine.Directory, "restore-orphan-*", "candidate", "snapshot.tfstate"))
	if err != nil || len(paths) == 0 {
		t.Fatal("restoration did not retain the uncheckpointed encrypted candidate")
	}
	found := false
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(data, interrupted) {
			found = true
		}
	}
	if !found {
		t.Fatal("restoration changed the retained uncheckpointed candidate bytes")
	}
	requirePrivateRecoveryFiles(t, engine)
}

func cancelAfterInverseSnapshot(t *testing.T, engine statemigration.Engine) []byte {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		defer func() {
			if failure := recover(); failure != nil {
				finished <- fmt.Errorf("restore worker panicked: %v", failure)
			}
		}()
		finished <- engine.Restore(ctx)
	}()
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(filepath.Join(engine.Directory, "restore-0000", "snapshot.tfstate"))
		if err == nil && len(data) != 0 {
			cancel()
			if err := <-finished; err == nil {
				t.Fatal("the inverse preparation completed after its cancellation boundary")
			}
			return data
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case err := <-finished:
			t.Fatalf("restoration stopped before creating its encrypted inverse candidate: %v", err)
		case <-timer.C:
			t.Fatal("restoration did not create its encrypted inverse candidate before timeout")
		case <-ticker.C:
		}
	}
}

func TestVerifyRestoredRefusesAChangedRecordedCandidate(t *testing.T) {
	engine, _ := restorationFixture(t)
	if err := engine.Execute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot := statemigration.Workspace{
		Directory:   filepath.Join(engine.Directory, "restore-0000"),
		Environment: engine.Workspaces["legacy"].Environment,
	}
	nativeCommand(t, snapshot, "state", "rm", "terraform_data.original")
	engine.CurrentCommit = engine.Manifest.RestoreCommit
	if err := engine.VerifyRestored(t.Context()); err == nil {
		t.Fatal("restoration verification ignored a changed recorded inverse candidate")
	}
	if err := migrationpause.Guard(engine.PauseFile); err == nil {
		t.Fatal("a changed inverse candidate released the infrastructure write pause")
	}
	requirePrivateRecoveryFiles(t, engine)
}

func TestRestoreStagesCollidingOriginalAddresses(t *testing.T) {
	engine, _ := migrationFixture(t)
	source := engine.OriginalWorkspaces["legacy"]
	extra, err := os.ReadFile(filepath.Join("testdata", "restore-extra.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.Directory, "extra.tf"), extra, 0o600); err != nil {
		t.Fatal(err)
	}
	nativeCommand(t, source, "apply", "-auto-approve", "-input=false", "-no-color")
	engine.Workspaces["legacy"] = source
	before := restoredState(t, engine, "legacy")
	engine.Manifest.Workspaces = engine.Manifest.Workspaces[:1]
	engine.Manifest.Workspaces[0].Before = before
	engine.Manifest.Moves = []statemigration.Transfer{
		{Source: "legacy", Address: "terraform_data.original", Destination: "legacy", NewAddress: "terraform_data.other"},
		{Source: "legacy", Address: "terraform_data.other", Destination: "legacy", NewAddress: "terraform_data.original"},
	}
	finalizeMigrationFixture(t, &engine)
	if err := engine.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Execute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := restoredState(t, engine, "legacy")
	if !reflect.DeepEqual(before.Records, after.Records) || before.Lineage != after.Lineage || after.Serial <= before.Serial {
		t.Fatal("restoration did not recover the original colliding addresses and provider records")
	}
	engine.CurrentCommit = engine.Manifest.RestoreCommit
	if err := engine.VerifyRestored(context.Background()); err != nil {
		t.Fatal(err)
	}
	requirePrivateRecoveryFiles(t, engine)
}

func TestRestorePreservesIdenticalOriginalDataRecords(t *testing.T) {
	engine, _ := migrationFixture(t)
	producer := workspaceFixture(t, "source")
	nativeCommand(t, producer, "apply", "-auto-approve", "-input=false", "-no-color")
	source := engine.OriginalWorkspaces["legacy"]
	source.Environment = append(slices.Clone(source.Environment), "TF_VAR_producer_state="+filepath.Join(producer.Directory, "runtime.tfstate"))
	configuration := readFixtureHCL(t, filepath.Join("testdata", "restore-identical.tf"))
	terraform := configuration.Body().FirstMatchingBlock("terraform", nil)
	if terraform == nil {
		t.Fatal("the identical-data fixture lacks its terraform block")
	}
	backend := terraform.Body().FirstMatchingBlock("backend", []string{"local"})
	if backend == nil {
		t.Fatal("the identical-data fixture lacks its local backend")
	}
	backend.Body().SetAttributeValue("path", cty.StringVal(fixtureBackendPath(t, source)))
	writeFixtureHCL(t, filepath.Join(source.Directory, "main.tf"), configuration)
	engine.Workspaces["legacy"] = source
	engine.OriginalWorkspaces["legacy"] = source
	nativeCommand(t, source, "apply", "-auto-approve", "-input=false", "-no-color")
	before := restoredState(t, engine, "legacy")
	first := "data.terraform_remote_state.first"
	second := "data.terraform_remote_state.second"
	if before.Records[first] == "" || before.Records[first] != before.Records[second] {
		t.Fatal("the real encrypted remote-state reads did not create identical original data records")
	}
	engine.Manifest.Workspaces = engine.Manifest.Workspaces[:1]
	engine.Manifest.Workspaces[0].Before = before
	engine.Manifest.Moves = []statemigration.Transfer{
		{Source: "legacy", Address: "terraform_data.original", Destination: "legacy", NewAddress: "terraform_data.original"},
		{Source: "legacy", Address: first, Destination: "legacy", NewAddress: second},
		{Source: "legacy", Address: second, Destination: "legacy", NewAddress: first},
	}
	finalizeMigrationFixture(t, &engine)
	if err := engine.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Execute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := restoredState(t, engine, "legacy")
	if !reflect.DeepEqual(before.Records, after.Records) {
		t.Fatal("restoration removed an original data record that matched another transferred record")
	}
	engine.CurrentCommit = engine.Manifest.RestoreCommit
	if err := engine.VerifyRestored(t.Context()); err != nil {
		t.Fatal(err)
	}
	requirePrivateRecoveryFiles(t, engine)
}
