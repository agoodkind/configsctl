//go:build unix

package main_test

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/statemigration"
)

func newLocalFileMigration(t *testing.T) (cliMigration, string) {
	t.Helper()
	root, err := os.MkdirTemp(shortTempRoot, "configs-local-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	migration := newCLIMigrationWithTree(t, newConfigsTreeAt(t, root))
	filename := filepath.Join(migration.private, "managed-file")
	source := strings.ReplaceAll(migrationFixtureFile(t, "baseline-original-local.tf"), "CONFIGSCTL_LOCAL_FILE", strconv.Quote(filename))
	writeFixtureFile(t, filepath.Join(migration.tree.workspaces, "legacy", "main.tf"), source)
	writeFixtureFile(t, filepath.Join(migration.tree.workspaces, "legacy", "provider.tf"), migrationFixtureFile(t, "baseline-local-provider.tf"))
	settings := strings.ReplaceAll(migrationFixtureFile(t, "settings.yml"), "[terraform_data]", "[terraform_data, local_file]")
	writeFixtureFile(t, filepath.Join(migration.tree.root, settingsName), settings)
	applyWorkspace(t, migration.tree, "legacy")
	mirror := filepath.Join(migration.private, "provider-mirror")
	args := tofuArgs("legacy", "providers", "mirror", mirror)
	requireSuccess(t, runConfigsctl(t, migration.tree, args...), args...)
	writeFixtureFile(t, filepath.Join(migration.tree.root, settingsName), settings+"  provider_mirror: "+strconv.Quote(mirror)+"\n")
	migration.manifest.RestoreCommit = commitMigrationFixture(t, migration.tree, "workspaces", settingsName)
	original := filepath.Join(migration.private, "file-original")
	fixtureGit(t, migration.tree, "-c", "core.hooksPath=/dev/null", "clone", "--no-hardlinks", "--", migration.tree.root, original)
	migration.manifest.RestoreConfigurationDirectory = original
	migration.manifest.Workspaces[0] = inspectCLIWorkspace(t, migration.tree, "legacy", filepath.Join(migration.private, "file-source-inspection"))
	cutover := strings.ReplaceAll(migrationFixtureFile(t, "baseline-cutover-local.tf"), "CONFIGSCTL_LOCAL_FILE", strconv.Quote(filename))
	writeFixtureFile(t, filepath.Join(migration.tree.workspaces, "legacy", "main.tf"), migrationFixtureFile(t, "retired.tf"))
	writeFixtureFile(t, filepath.Join(migration.tree.workspaces, "application", "main.tf"), cutover)
	writeFixtureFile(t, filepath.Join(migration.tree.workspaces, "application", "provider.tf"), migrationFixtureFile(t, "baseline-local-provider.tf"))
	args = tofuArgs("application", "init", "-input=false")
	requireSuccess(t, runConfigsctl(t, migration.tree, args...), args...)
	migration.manifest.Commit = commitMigrationFixture(t, migration.tree, "workspaces")
	migration.manifest.Moves = append(migration.manifest.Moves, statemigration.Transfer{Source: "legacy", Address: "local_file.original_file", Destination: "application", NewAddress: "local_file.migrated_file"})
	migration.writeManifest(t)
	return migration, filename
}

func TestMigrationBaselinePreservesNativeRefreshDriftAndRejectsChangedBaseline(t *testing.T) {
	migration, filename := newLocalFileMigration(t)
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	external := migrationFixtureValue + "-external"
	writeFixtureFile(t, filename, external)
	before := readPreflightSource(t, migration)
	requireSuccess(t, migration.run(t, "preflight"), "migrate-state", "preflight")
	requirePreflightUnchanged(t, migration, before)
	baseline := readCLIBaseline(t, migration)
	if !baseline.Forward || !slices.ContainsFunc(baseline.Original, func(proof cliPlanEntryProof) bool { return proof.Kind == "drift" }) {
		t.Fatal("the native baseline omitted the actual local-file refresh drift")
	}
	writeFixtureFile(t, filename, migrationFixtureValue)
	refused := migration.run(t, "execute")
	if refused.exitCode == 0 || !strings.Contains(refused.stderr, "frozen migration baseline") {
		t.Fatalf("execution replaced its recorded native drift baseline: %s", refused.stderr)
	}
	comparison := readCLIComparison(t, migration)
	if !slices.ContainsFunc(comparison.Differences, func(difference cliComparisonDifference) bool {
		return difference.Kind == "drift" && difference.Classification == "removed" && difference.Before != "" && difference.After == ""
	}) {
		t.Fatal("the frozen comparison omitted the removed native refresh drift identity")
	}
	requirePreflightUnchanged(t, migration, before)
	writeFixtureFile(t, filename, external)
	requireSuccess(t, migration.run(t, "execute"), "migrate-state", "execute")
	requireSuccess(t, migration.run(t, "verify"), "migrate-state", "verify")
	actual, err := os.ReadFile(filename)
	if err != nil || !bytes.Equal(actual, []byte(external)) {
		t.Fatal("the migration applied the pending local-file repair")
	}
	state := inspectCLIWorkspace(t, migration.tree, "application", filepath.Join(migration.private, "file-final-inspection"))
	if state.Before.Records["local_file.migrated_file"] != migration.manifest.Workspaces[0].Before.Records["local_file.original_file"] {
		t.Fatal("the native migration changed the complete local-file provider record")
	}
	requireEncryptedMigrationFiles(t, migration.directory)
}
