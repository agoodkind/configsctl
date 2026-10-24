//go:build unix

package main_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/migrationpause"
)

func TestMigrationCLITransferCompletesProtectedNativeStages(t *testing.T) {
	t.Parallel()
	migration := newCLIMigration(t)
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	args := []string{"migrate-state", "transfer", migration.file, migration.directory, "--parallelism=1"}
	result := runConfigsctl(t, migration.tree, args...)
	requireSuccess(t, result, args...)
	requireMigrationPrivateOutput(t, result)
	if migrationCLIStateIdentity(t, migration.tree, "application", applicationInstance) != migration.identity {
		t.Fatal("the complete transfer changed the native instance identity")
	}
	after := inspectCLIWorkspace(t, migration.tree, "application", filepath.Join(migration.private, "complete-transfer"))
	if after.Before.Records[applicationInstance] != migration.manifest.Workspaces[0].Before.Records[legacyInstance] {
		t.Fatal("the complete transfer changed the native instance or provider state")
	}
	apply := tofuArgs("application", "apply", "-auto-approve", "-input=false")
	requireSuccess(t, runConfigsctl(t, migration.tree, apply...), apply...)
	requireEncryptedMigrationFiles(t, migration.directory)
	requirePreflightCleanup(t, migration)
}

func TestMigrationCLITransferRefusesUnrelatedPauseBeforeNativeOperations(t *testing.T) {
	migration := newCLIMigration(t)
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	before := readPreflightSource(t, migration)
	t.Setenv("HOME", migration.tree.home)
	t.Setenv("XDG_CONFIG_HOME", "")
	path, err := migrationpause.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	pause := []byte(`{"manifest":"unrelated-reviewed-migration","directory":"/unrelated-private-migration"}`)
	if err := os.WriteFile(path, pause, 0o600); err != nil {
		t.Fatal(err)
	}
	result := migration.run(t, "transfer")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "another migration owns the infrastructure write pause") {
		t.Fatal("transfer accepted an unrelated durable migration pause")
	}
	if strings.Contains(result.stderr, "migration.native.started") {
		t.Fatal("transfer started a native command before refusing another pause owner")
	}
	if !bytes.Equal(before, readPreflightSource(t, migration)) {
		t.Fatal("the refused transfer changed the original backend state")
	}
	requireNoFile(t, filepath.Join(migration.tree.workspaces, "application", "runtime.tfstate"))
	requirePreflightCleanup(t, migration)
	retained, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(retained, pause) {
		t.Fatal("the refused transfer replaced or removed the unrelated pause")
	}
}

func TestMigrationCLIVerifyRefusesMissingPauseBeforeCredentialActivation(t *testing.T) {
	t.Parallel()
	migration := newCLIMigration(t)
	configureMigrationCredentialRefusal(t, &migration, false)
	original := originalMigrationTree(migration)
	for _, fixture := range []struct {
		tree     configsTree
		original bool
	}{
		{tree: original, original: true},
		{tree: migration.tree, original: false},
	} {
		writeFixtureFile(t, filepath.Join(fixture.tree.root, "ansible", "ansible.cfg"), lockedAnsibleConfig)
		writeFixtureFile(t, filepath.Join(fixture.tree.root, "ansible", "inventory", "hosts.yml"), "all:\n  hosts:\n    synthetic-lock-host:\n      ansible_host: 127.0.0.1\n      ansible_user: root\n      ansible_connection: ssh\n")
		commit := commitMigrationFixture(t, fixture.tree, "ansible")
		if fixture.original {
			migration.manifest.RestoreCommit = commit
		} else {
			migration.manifest.Commit = commit
		}
	}
	migration.writeManifest(t)
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	journal := filepath.Join(migration.directory, "journal.json")
	data, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	verified := strings.Replace(string(data), `"phase": "prepared"`, `"phase": "verified"`, 1)
	if verified == string(data) {
		t.Fatal("the native fixture did not retain its prepared phase")
	}
	if err := os.WriteFile(journal, []byte(verified), 0o600); err != nil {
		t.Fatal(err)
	}
	before := readPreflightSource(t, migration)
	result := runTofuEnvironment(t, migration.tree, "", []string{"TF_VAR_fixture_required=" + migrationFixtureValue}, "migrate-state", "verify", migration.file, migration.directory)
	requireMigrationPrivateOutput(t, result)
	if result.exitCode == 0 || !strings.Contains(result.stderr, "read the required infrastructure write pause") {
		t.Fatalf("verification did not refuse its missing durable pause before activation: %s", result.stderr)
	}
	if strings.Contains(result.stderr, "migration.native.started") || strings.Contains(result.stderr, "migration.credential.activation_failed") {
		t.Fatal("verification started a native or credential activation operation without its pause")
	}
	requirePreflightUnchanged(t, migration, before)
}

func TestMigrationCLITransferRefusesUnsupportedJournalPhaseBeforeNativeOperations(t *testing.T) {
	t.Parallel()
	migration := newCLIMigration(t)
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	path := filepath.Join(migration.directory, "journal.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), `"phase": "prepared"`, `"phase": "unsupported-phase"`, 1)
	if changed == string(data) {
		t.Fatal("the native fixture did not retain its prepared phase")
	}
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	result := migration.run(t, "transfer")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "cannot run migration transfer in phase unsupported-phase") {
		t.Fatal("transfer did not refuse the unsupported journal phase")
	}
	if strings.Contains(result.stderr, "migration.native.started") {
		t.Fatal("transfer started a native command before refusing its journal phase")
	}
	if migrationCLIStateIdentity(t, migration.tree, "legacy", legacyInstance) != migration.identity {
		t.Fatal("the refused transfer changed the original native identity")
	}
}

func TestMigrationCLITransferRejectsInvalidParallelismBeforeCommands(t *testing.T) {
	t.Parallel()
	tree := newConfigsTree(t)
	result := runConfigsctl(t, tree, "migrate-state", "transfer", "absent-manifest", filepath.Join(t.TempDir(), "private"), "--parallelism=0")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "positive decimal integer") || strings.Contains(result.stderr, "migration.command.started") {
		t.Fatal("transfer accepted invalid typed plan parallelism or started a command")
	}
}
