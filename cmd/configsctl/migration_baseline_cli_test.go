//go:build unix

package main_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func originalMigrationTree(migration cliMigration) configsTree {
	root := migration.manifest.RestoreConfigurationDirectory
	return configsTree{root: root, home: migration.tree.home, workspaces: filepath.Join(root, "workspaces")}
}

func configurePendingMigration(t *testing.T, migration *cliMigration, cutover string) {
	t.Helper()
	original := originalMigrationTree(*migration)
	values := "vault_tofu_state_passphrase: " + migrationFixtureKey + "\nvault_migration_fixture: " + migrationFixtureValue + "\nvault_source_only: " + migrationFixtureValue + "-pending\n"
	requireSuccess(t, runMutationInput(t, original, values, "set-secrets"), "set-secrets")
	writeFixtureFile(t, filepath.Join(original.workspaces, "legacy", "main.tf"), migrationFixtureFile(t, "baseline-original.tf"))
	migration.manifest.RestoreCommit = commitMigrationFixture(t, original, "workspaces/legacy/main.tf", "ansible")
	if cutover == "destination.tf" {
		migration.writeManifest(t)
		return
	}
	updatePreflightFixture(t, migration, migrationFixtureFile(t, cutover))
}

func TestMigrationBaselinePreservesPendingUpdateWithoutApplyingIt(t *testing.T) {
	migration := newCLIMigration(t)
	configurePendingMigration(t, &migration, "baseline-pending.tf")
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	before := readPreflightSource(t, migration)
	requireSuccess(t, migration.run(t, "preflight"), "migrate-state", "preflight")
	requirePreflightUnchanged(t, migration, before)
	baseline := readCLIBaseline(t, migration)
	if !baseline.Forward || len(baseline.Original) != 1 || len(baseline.Cutover) != 1 || baseline.Original[0].Digest != baseline.Cutover[0].Digest {
		t.Fatal("preflight did not retain equal original and cutover native action proofs")
	}
	for _, action := range []string{"execute", "resume", "verify"} {
		requireSuccess(t, migration.run(t, action), "migrate-state", action)
	}
	actual := inspectCLIWorkspace(t, migration.tree, "application", filepath.Join(migration.private, "pending-final"))
	if actual.Before.Records[applicationInstance] != migration.manifest.Workspaces[0].Before.Records[legacyInstance] {
		t.Fatal("the native migration applied the pending infrastructure update")
	}
	if migrationCLIStateIdentity(t, migration.tree, "application", applicationInstance) != migration.identity {
		t.Fatal("the native pending-action migration changed the instance ID")
	}
	requireEncryptedMigrationFiles(t, migration.directory)
}

type cliPlanBaseline struct {
	Forward  bool                `json:"forward_verified"`
	Original []cliPlanEntryProof `json:"original"`
	Cutover  []cliPlanEntryProof `json:"cutover"`
}

type cliPlanEntryProof struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
}

func readCLIBaseline(t *testing.T, migration cliMigration) cliPlanBaseline {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(migration.directory, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	var journal struct {
		Baseline cliPlanBaseline `json:"plan_baseline"`
	}
	if err := json.Unmarshal(data, &journal); err != nil {
		t.Fatal(err)
	}
	return journal.Baseline
}

func TestMigrationBaselineRefusesChangedOrRemovedPendingUpdate(t *testing.T) {
	for _, fixture := range []string{"baseline-changed.tf", "destination.tf"} {
		t.Run(fixture, func(t *testing.T) {
			migration := newCLIMigration(t)
			configurePendingMigration(t, &migration, fixture)
			requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
			before := readPreflightSource(t, migration)
			refused := migration.run(t, "preflight")
			if refused.exitCode == 0 || !strings.Contains(refused.stderr, "migration changes the original managed plan") {
				t.Fatalf("preflight accepted a changed or removed pending action: %s", refused.stderr)
			}
			if readCLIBaseline(t, migration).Forward {
				t.Fatal("a failed comparison recorded a successful forward baseline")
			}
			comparison := readCLIComparison(t, migration)
			if comparison.Status != "different" || len(comparison.Differences) != 1 {
				t.Fatal("the failed native comparison omitted its safe identity metadata")
			}
			difference := comparison.Differences[0]
			if difference.Kind != "change" || difference.Workspace != "application" || difference.Address != applicationInstance || difference.Classification != "changed" || len(difference.Before) != 64 || len(difference.After) != 64 || difference.Before == difference.After {
				t.Fatal("the failed comparison omitted its exact changed identity or complete old/new hashes")
			}
			if !strings.Contains(refused.stderr, "change terraform_data.renamed in application") {
				t.Fatal("the public refusal omitted the first differing native plan identity")
			}
			requirePreflightUnchanged(t, migration, before)
		})
	}
}

type cliComparisonDifference struct {
	Kind           string `json:"kind"`
	Workspace      string `json:"workspace"`
	Address        string `json:"address"`
	Classification string `json:"classification"`
	Before         string `json:"old_digest"`
	After          string `json:"new_digest"`
}

type cliComparisonReport struct {
	Status      string                    `json:"status"`
	Differences []cliComparisonDifference `json:"differences"`
}

func readCLIComparison(t *testing.T, migration cliMigration) cliComparisonReport {
	t.Helper()
	path := filepath.Join(migration.directory, "reports", "baseline-comparison.json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("the native comparison metadata lacks mode 0600")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{migrationFixtureValue, migrationFixtureKey} {
		if bytes.Contains(data, []byte(marker)) {
			t.Fatal("the native comparison metadata exposed a private value")
		}
	}
	var report cliComparisonReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestMigrationBaselineRefusesOriginalNativeErrors(t *testing.T) {
	migration := newCLIMigration(t)
	original := originalMigrationTree(migration)
	writeFixtureFile(t, filepath.Join(original.workspaces, "legacy", "main.tf"), migrationFixtureFile(t, "baseline-original-error.tf"))
	migration.manifest.RestoreCommit = commitMigrationFixture(t, original, "workspaces/legacy/main.tf")
	migration.writeManifest(t)
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	before := readPreflightSource(t, migration)
	refused := migration.run(t, "preflight")
	if refused.exitCode == 0 {
		t.Fatal("preflight treated an original native validation failure as an empty plan")
	}
	report := readCLIPlanReport(t, migration, "baseline-original-0000.json")
	if report.Status != "native_plan_failed" || len(report.Diagnostics) != 1 {
		t.Fatal("preflight omitted the original native error proof")
	}
	requirePreflightUnchanged(t, migration, before)
}

func TestMigrationBaselinePreservesReplacementOrder(t *testing.T) {
	for _, fixture := range []string{"baseline-replacement.tf", "baseline-replacement-order.tf"} {
		t.Run(fixture, func(t *testing.T) {
			migration := newCLIMigration(t)
			original := originalMigrationTree(migration)
			writeFixtureFile(t, filepath.Join(original.workspaces, "legacy", "main.tf"), migrationFixtureFile(t, "baseline-original-replacement.tf"))
			migration.manifest.RestoreCommit = commitMigrationFixture(t, original, "workspaces/legacy/main.tf")
			updatePreflightFixture(t, &migration, migrationFixtureFile(t, fixture))
			requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
			before := readPreflightSource(t, migration)
			result := migration.run(t, "preflight")
			if fixture == "baseline-replacement.tf" {
				requireSuccess(t, result, "migrate-state", "preflight")
			} else if result.exitCode == 0 || !strings.Contains(result.stderr, "migration changes the original managed plan") {
				t.Fatal("preflight ignored a changed native replacement order")
			}
			requirePreflightUnchanged(t, migration, before)
		})
	}
}

func TestMigrationBaselineRefusesUnmappedPendingImport(t *testing.T) {
	migration := newCLIMigration(t)
	original := originalMigrationTree(migration)
	writeFixtureFile(t, filepath.Join(original.workspaces, "legacy", "main.tf"), migrationFixtureFile(t, "baseline-original-import.tf"))
	migration.manifest.RestoreCommit = commitMigrationFixture(t, original, "workspaces/legacy/main.tf")
	migration.writeManifest(t)
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	before := readPreflightSource(t, migration)
	refused := migration.run(t, "preflight")
	if refused.exitCode == 0 || !strings.Contains(refused.stderr, "unmapped managed instance terraform_data.unmapped_import in legacy") {
		t.Fatalf("preflight adopted an unapplied native import: %s", refused.stderr)
	}
	report := readCLIPlanReport(t, migration, "baseline-original-0000.json")
	if report.Status != "managed_changes" || len(report.Changes) != 1 || report.Changes[0].Address != "terraform_data.unmapped_import" {
		t.Fatal("the native no-op import did not appear in the managed change report")
	}
	if !slices.Equal(report.Changes[0].Actions, []string{"no-op"}) {
		t.Fatal("the native fixture did not produce a pending no-op import")
	}
	for _, name := range []string{"baseline-original-0000.json", "preflight-0000.json", "preflight-0001.json"} {
		data, err := os.ReadFile(filepath.Join(migration.directory, "reports", name))
		if err != nil || bytes.Contains(data, []byte("synthetic-private-import-identity-never-print")) {
			t.Fatal("the plan metadata exposed a private import identity")
		}
	}
	requirePreflightUnchanged(t, migration, before)
}

func TestMigrationBaselinePreservesOnlyReviewedPendingMoveTarget(t *testing.T) {
	for _, target := range []string{"terraform_data.pending", legacyInstance} {
		t.Run(target, func(t *testing.T) {
			migration := newCLIMigration(t)
			original := originalMigrationTree(migration)
			writeFixtureFile(t, filepath.Join(original.workspaces, "legacy", "main.tf"), migrationFixtureFile(t, "baseline-original-moved.tf"))
			migration.manifest.RestoreCommit = commitMigrationFixture(t, original, "workspaces/legacy/main.tf")
			configuration := migrationFixtureFile(t, "baseline-pending-target.tf")
			if target == legacyInstance {
				configuration = migrationFixtureFile(t, "source.tf")
			}
			migration.manifest.Moves[0].NewAddress = target
			updatePreflightFixture(t, &migration, configuration)
			requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
			before := readPreflightSource(t, migration)
			result := migration.run(t, "preflight")
			if target == "terraform_data.pending" {
				requireSuccess(t, result, "migrate-state", "preflight")
			} else if result.exitCode == 0 || !strings.Contains(result.stderr, "pending native move target differs") {
				t.Fatal("preflight erased a pending native move beyond the reviewed mapping")
			}
			report := readCLIPlanReport(t, migration, "baseline-original-0000.json")
			if report.Status != "managed_changes" || len(report.Changes) != 1 || !slices.Equal(report.Changes[0].Actions, []string{"no-op"}) {
				t.Fatal("the real native fixture did not report its pending no-op move")
			}
			requirePreflightUnchanged(t, migration, before)
		})
	}
}

func TestMigrationBaselineRestoresAfterRejectedForwardComparison(t *testing.T) {
	migration := newCLIMigration(t)
	configurePendingMigration(t, &migration, "baseline-changed.tf")
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	before := readPreflightSource(t, migration)
	if migration.run(t, "preflight").exitCode == 0 {
		t.Fatal("the forward comparison accepted different pending values")
	}
	requirePreflightUnchanged(t, migration, before)
	caller := migration
	caller.tree = originalMigrationTree(migration)
	requireSuccess(t, caller.run(t, "restore"), "migrate-state", "restore")
	requireMigrationPaused(t, caller.tree, "legacy")
	requireSuccess(t, caller.run(t, "verify-restored"), "migrate-state", "verify-restored")
	baseline := readCLIBaseline(t, migration)
	if baseline.Forward || len(baseline.Original) != 1 || len(baseline.Cutover) != 0 {
		t.Fatal("recovery did not derive only its immutable original native plan proof")
	}
	actual := inspectCLIWorkspace(t, migration.tree, "legacy", filepath.Join(migration.private, "recovered-original"))
	if actual.Before.Records[legacyInstance] != migration.manifest.Workspaces[0].Before.Records[legacyInstance] {
		t.Fatal("recovery applied the original pending infrastructure change")
	}
	requireEncryptedMigrationFiles(t, migration.directory)
}

func TestMigrationBaselineRefusesAnUnreviewedCutoverMoveTarget(t *testing.T) {
	migration := newCLIMigration(t)
	updatePreflightFixture(t, &migration, migrationFixtureFile(t, "baseline-cutover-moved.tf"))
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	before := readPreflightSource(t, migration)
	refused := migration.run(t, "preflight")
	if refused.exitCode == 0 || !strings.Contains(refused.stderr, "pending native move target differs") {
		t.Fatal("preflight erased an unreviewed cutover move target")
	}
	original := readCLIPlanReport(t, migration, "baseline-original-0000.json")
	cutover := readCLIPlanReport(t, migration, "preflight-0001.json")
	if original.Status != "passed" || cutover.Status != "managed_changes" || len(cutover.Changes) != 1 || !slices.Equal(cutover.Changes[0].Actions, []string{"no-op"}) {
		t.Fatal("the real native fixture did not isolate the changed cutover move target")
	}
	requirePreflightUnchanged(t, migration, before)
}
