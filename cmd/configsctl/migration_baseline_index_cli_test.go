//go:build unix

package main_test

import (
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/statemigration"
)

const (
	indexedSourceAddress      = `module.old["blue"].terraform_data.entry["old"]`
	indexedDestinationAddress = `module.new[0].terraform_data.renamed["new"]`
	exactBaselineNumber       = "9007199254740992"
)

func newIndexedMigration(t *testing.T, destinationNumber string) cliMigration {
	t.Helper()
	migration := newCLIMigration(t)
	for _, module := range []struct{ directory, fixture string }{
		{"indexed-source", "baseline-indexed-source-module.tf"},
		{"indexed-destination", "baseline-indexed-destination-module.tf"},
	} {
		writeFixtureFile(t, filepath.Join(migration.tree.root, "modules", module.directory, "main.tf"), migrationFixtureFile(t, module.fixture))
	}
	source := migrationFixtureFile(t, "baseline-indexed-source.tf")
	writeFixtureFile(t, filepath.Join(migration.tree.workspaces, "legacy", "main.tf"), strings.ReplaceAll(source, "CONFIGSCTL_NUMBER", "1"))
	applyWorkspace(t, migration.tree, "legacy")
	migration.identity = migrationCLIStateIdentity(t, migration.tree, "legacy", indexedSourceAddress)
	migration.manifest.Workspaces[0] = inspectCLIWorkspace(t, migration.tree, "legacy", filepath.Join(migration.private, "indexed-source-inspection"))
	writeFixtureFile(t, filepath.Join(migration.tree.workspaces, "legacy", "main.tf"), strings.ReplaceAll(source, "CONFIGSCTL_NUMBER", exactBaselineNumber))
	migration.manifest.RestoreCommit = commitMigrationFixture(t, migration.tree, "workspaces", "modules")
	original := filepath.Join(migration.private, "indexed-original")
	fixtureGit(t, migration.tree, "-c", "core.hooksPath=/dev/null", "clone", "--no-hardlinks", "--", migration.tree.root, original)
	migration.manifest.RestoreConfigurationDirectory = original
	writeFixtureFile(t, filepath.Join(migration.tree.workspaces, "legacy", "main.tf"), migrationFixtureFile(t, "retired.tf"))
	destination := strings.ReplaceAll(migrationFixtureFile(t, "baseline-indexed-destination.tf"), "CONFIGSCTL_NUMBER", destinationNumber)
	writeFixtureFile(t, filepath.Join(migration.tree.workspaces, "application", "main.tf"), destination)
	migration.manifest.Commit = commitMigrationFixture(t, migration.tree, "workspaces")
	migration.manifest.Moves = []statemigration.Transfer{{Source: "legacy", Address: indexedSourceAddress, Destination: "application", NewAddress: indexedDestinationAddress}}
	migration.writeManifest(t)
	return migration
}

func TestMigrationBaselineNormalizesIndexedIdentityAndPreservesExactNumbers(t *testing.T) {
	for _, number := range []string{exactBaselineNumber, "9007199254740993"} {
		t.Run(number, func(t *testing.T) {
			migration := newIndexedMigration(t, number)
			requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
			before := readPreflightSource(t, migration)
			result := migration.run(t, "preflight")
			if number != exactBaselineNumber {
				if result.exitCode == 0 || !strings.Contains(result.stderr, "migration changes the original managed plan") {
					t.Fatal("the comparison rounded distinct native numbers above 2^53")
				}
				requirePreflightUnchanged(t, migration, before)
				return
			}
			requireSuccess(t, result, "migrate-state", "preflight")
			requirePreflightUnchanged(t, migration, before)
			requireSuccess(t, migration.run(t, "execute"), "migrate-state", "execute")
			requireSuccess(t, migration.run(t, "verify"), "migrate-state", "verify")
			actual := inspectCLIWorkspace(t, migration.tree, "application", filepath.Join(migration.private, "indexed-final-inspection"))
			if actual.Before.Records[indexedDestinationAddress] != migration.manifest.Workspaces[0].Before.Records[indexedSourceAddress] {
				t.Fatal("the indexed migration changed provider attributes or applied the pending numeric update")
			}
			if migrationCLIStateIdentity(t, migration.tree, "application", indexedDestinationAddress) != migration.identity {
				t.Fatal("the indexed migration changed the original resource ID")
			}
			requireEncryptedMigrationFiles(t, migration.directory)
		})
	}
}
