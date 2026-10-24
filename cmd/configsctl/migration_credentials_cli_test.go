//go:build unix

package main_test

import (
	"path/filepath"
	"strings"
	"testing"
)

func configureMigrationCredentialRefusal(t *testing.T, migration *cliMigration, changedAccount bool) {
	t.Helper()
	original := originalMigrationTree(*migration)
	for _, fixture := range []struct {
		tree     configsTree
		selector string
		original bool
	}{
		{tree: original, selector: "legacy", original: true},
		{tree: migration.tree, selector: "application", original: false},
	} {
		settings := strings.ReplaceAll(migrationFixtureFile(t, "credential-settings.yml"), "MIGRATION_CREDENTIAL_SELECTOR", fixture.selector)
		if changedAccount && !fixture.original {
			settings = strings.Replace(settings, `account_id: "3"`, `account_id: "4"`, 1)
		}
		writeFixtureFile(t, filepath.Join(fixture.tree.root, settingsName), settings)
		writeFixtureFile(t, filepath.Join(fixture.tree.workspaces, fixture.selector, "credential-inputs.tf"), migrationFixtureFile(t, "credential-inputs.tf"))
		result := runMutationInput(t, fixture.tree, "vault_migration_rotator: "+migrationFixtureValue+"-rotator\n", "set-secrets")
		requireSuccess(t, result, "set-secrets")
		commit := commitMigrationFixture(t, fixture.tree, settingsName, "workspaces", "ansible")
		if fixture.original {
			migration.manifest.RestoreCommit = commit
		} else {
			migration.manifest.Commit = commit
		}
	}
	migration.writeManifest(t)
}

func TestMigrationCLICredentialsRefuseMissingRequiredEphemeralInputBeforeActivation(t *testing.T) {
	migration := newCLIMigration(t)
	configureMigrationCredentialRefusal(t, &migration, false)
	result := migration.run(t, "transfer")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "required migration input fixture_required is missing") {
		t.Fatal("the protected migration did not refuse its missing required ephemeral input")
	}
	if strings.Contains(result.stderr, "migration.native.started") || strings.Contains(result.stderr, "migration.credential.activation_failed") {
		t.Fatal("the missing migration input started a native or credential activation operation")
	}
	requireNoFile(t, filepath.Join(migration.directory, "journal.json"))
	if migrationCLIStateIdentity(t, migration.tree, "legacy", legacyInstance) != migration.identity {
		t.Fatal("the missing input changed the native source identity")
	}
}

func TestMigrationCLICredentialsRefuseMismatchedAccountsBeforeActivation(t *testing.T) {
	migration := newCLIMigration(t)
	configureMigrationCredentialRefusal(t, &migration, true)
	result := migration.run(t, "transfer")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "does not preserve its exact configured provider credential adapter") {
		t.Fatal("the protected migration accepted mismatched reviewed account identities")
	}
	if strings.Contains(result.stderr, "migration.native.started") || strings.Contains(result.stderr, "migration.credential.activation_failed") {
		t.Fatal("the mismatched account started a native or credential activation operation")
	}
	requireNoFile(t, filepath.Join(migration.directory, "journal.json"))
}

func TestMigrationCLICredentialsRefuseUnsafeLoggingBeforeActivation(t *testing.T) {
	migration := newCLIMigration(t)
	configureMigrationCredentialRefusal(t, &migration, false)
	result := runTofuEnvironment(t, migration.tree, "", []string{"TF_LOG=TRACE"}, "migrate-state", "transfer", migration.file, migration.directory)
	requireMigrationPrivateOutput(t, result)
	if result.exitCode == 0 || !strings.Contains(result.stderr, "native logging and profiling overrides to be disabled") {
		t.Fatal("the protected migration accepted unsafe provider logging")
	}
	if strings.Contains(result.stderr, "migration.native.started") || strings.Contains(result.stderr, "migration.credential.activation_failed") {
		t.Fatal("unsafe logging started a native or credential activation operation")
	}
}
