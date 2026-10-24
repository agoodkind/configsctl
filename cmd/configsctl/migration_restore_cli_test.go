//go:build unix

package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/statemigration"
)

const retainedMigrationMirror = "retained-mirror"

func newSeparateCheckoutMigration(t *testing.T) (cliMigration, configsTree) {
	t.Helper()
	original := newConfigsTree(t)
	values := "vault_tofu_state_passphrase: " + migrationFixtureKey + "\nvault_migration_fixture: " + migrationFixtureValue + "\n"
	requireSuccess(t, runMutationInput(t, original, values, "set-secrets"), "set-secrets")
	writeFixtureFile(t, filepath.Join(original.root, settingsName), migrationFixtureFile(t, "settings.yml"))
	writeFixtureFile(t, filepath.Join(original.root, ".gitignore"), migrationFixtureFile(t, "ignore.txt"))
	private := t.TempDir()
	backend := func(selector string) string {
		return strings.Replace(migrationFixtureFile(t, "backend.tf"), `"runtime.tfstate"`, strconv.Quote(filepath.Join(private, selector+".tfstate")), 1)
	}
	writeFixtureFile(t, filepath.Join(original.workspaces, "legacy", "backend.tf"), backend("legacy"))
	writeFixtureFile(t, filepath.Join(original.workspaces, "legacy", "main.tf"), migrationFixtureFile(t, "source.tf"))
	fixtureGit(t, original, "init", "--initial-branch=main")
	recovery := commitMigrationFixture(t, original, ".gitignore", settingsName, "workspaces", "ansible")
	applyWorkspace(t, original, "legacy")
	identity := migrationCLIStateIdentity(t, original, "legacy", legacyInstance)
	cutoverRoot := filepath.Join(t.TempDir(), "retained")
	fixtureGit(t, original, "-c", "core.hooksPath=/dev/null", "clone", "--no-hardlinks", "--", original.root, cutoverRoot)
	cutover := configsTree{root: cutoverRoot, home: original.home, workspaces: filepath.Join(cutoverRoot, "workspaces")}
	settings := migrationFixtureFile(t, "settings.yml") + "  provider_mirror: " + retainedMigrationMirror + "\n"
	writeFixtureFile(t, filepath.Join(cutover.root, settingsName), settings)
	writeFixtureFile(t, filepath.Join(cutover.root, retainedMigrationMirror, "tofu.example.test", "example", "local", ".keep"), "")
	writeFixtureFile(t, filepath.Join(cutover.workspaces, "legacy", "main.tf"), migrationFixtureFile(t, "retired.tf"))
	writeFixtureFile(t, filepath.Join(cutover.workspaces, "application", "backend.tf"), backend("application"))
	writeFixtureFile(t, filepath.Join(cutover.workspaces, "application", "main.tf"), migrationFixtureFile(t, "destination.tf"))
	commit := commitMigrationFixture(t, cutover, "workspaces", settingsName, retainedMigrationMirror)
	for _, selector := range []string{"legacy", "application"} {
		args := tofuArgs(selector, "init", "-input=false")
		requireSuccess(t, runConfigsctl(t, cutover, args...), args...)
	}
	source := inspectCLIWorkspace(t, cutover, "legacy", filepath.Join(private, "source-inspection"))
	destination := inspectCLIWorkspace(t, cutover, "application", filepath.Join(private, "destination-inspection"))
	manifest := statemigration.Manifest{
		Version: 1, Commit: commit, RestoreCommit: recovery, ConfigurationDirectory: cutover.root,
		Workspaces: []statemigration.WorkspaceSpec{source, destination},
		Moves: []statemigration.Transfer{{
			Source: "legacy", Address: legacyInstance,
			Destination: "application", NewAddress: applicationInstance,
		}},
	}
	migration := cliMigration{
		tree: cutover, private: private, directory: filepath.Join(private, "transfer"),
		manifest: manifest, file: filepath.Join(private, "manifest.json"), identity: identity,
	}
	migration.writeManifest(t)
	return migration, original
}

func requireMigrationPaused(t *testing.T, tree configsTree, selector string) {
	t.Helper()
	result := runConfigsctl(t, tree, tofuArgs(selector, "apply", "-auto-approve", "-input=false")...)
	requireMigrationPrivateOutput(t, result)
	if result.exitCode == 0 || !strings.Contains(result.stderr, "writes remain paused") {
		t.Fatalf("the incomplete restoration allowed an ordinary apply: %s", result.stderr)
	}
}

func retainCLIPauseAfterRestorationVerification(t *testing.T, migration cliMigration) {
	t.Helper()
	var pauses []string
	err := filepath.WalkDir(migration.tree.home, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Name() == "pause.json" {
			pauses = append(pauses, path)
		}
		return nil
	})
	if err != nil || len(pauses) != 1 {
		t.Fatalf("the real migration did not create one durable pause: %v", err)
	}
	pause, err := os.ReadFile(pauses[0])
	if err != nil {
		t.Fatal(err)
	}
	requireSuccess(t, migration.run(t, "verify-restored"), "migrate-state", "verify-restored")
	requireNoFile(t, pauses[0])
	// Retain the exact pause bytes to reproduce interruption after the terminal checkpoint.
	writeFixtureFile(t, pauses[0], string(pause))
	requireMigrationPaused(t, migration.tree, "legacy")
}

func requireCutoverCheckoutForRestoration(t *testing.T, migration, caller cliMigration) {
	t.Helper()
	caller.manifest.ConfigurationDirectory = caller.tree.root
	caller.writeManifest(t)
	refused := caller.run(t, "restore")
	if refused.exitCode == 0 || !strings.Contains(refused.stderr, "retained cutover checkout does not match") {
		t.Fatalf("recovery accepted an unrelated retained checkout: %s", refused.stderr)
	}
	migration.writeManifest(t)
	requireMigrationPaused(t, caller.tree, "legacy")
	settings := filepath.Join(migration.tree.root, settingsName)
	reviewedSettings, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, settings, "invalid: [")
	refused = caller.run(t, "restore")
	if refused.exitCode == 0 || !strings.Contains(refused.stderr, "commit the reviewed configuration changes") {
		t.Fatalf("recovery trusted settings from a dirty retained checkout: %s", refused.stderr)
	}
	writeFixtureFile(t, settings, string(reviewedSettings))
}

func TestMigrationCLIRestoresUsingRetainedCutoverAndOriginalCheckout(t *testing.T) {
	migration, original := newSeparateCheckoutMigration(t)
	requireNoFile(t, filepath.Join(original.workspaces, "application"))
	requireNoFile(t, filepath.Join(original.root, retainedMigrationMirror))
	for _, operation := range []string{"prepare", "execute"} {
		requireSuccess(t, migration.run(t, operation), "migrate-state", operation)
	}
	if migrationCLIStateIdentity(t, migration.tree, "application", applicationInstance) != migration.identity {
		t.Fatal("the cutover changed the original instance ID")
	}
	caller := migration
	caller.tree = original
	requireCutoverCheckoutForRestoration(t, migration, caller)
	requireSuccess(t, caller.run(t, "restore"), "migrate-state", "restore")
	if migrationCLIStateIdentity(t, original, "legacy", legacyInstance) != migration.identity {
		t.Fatal("restoration changed the original instance ID")
	}
	source := inspectCLIWorkspace(t, original, "legacy", filepath.Join(migration.private, "restored-source"))
	if source.Before.Records[legacyInstance] != migration.manifest.Workspaces[0].Before.Records[legacyInstance] {
		t.Fatal("restoration changed the original provider record")
	}
	actual := inspectCLIWorkspace(t, migration.tree, "application", filepath.Join(migration.private, "restored-destination"))
	if len(actual.Before.Records) != 0 {
		t.Fatal("restoration retained destination resource ownership")
	}
	requireMigrationPaused(t, original, "legacy")
	refused := migration.run(t, "verify-restored")
	if refused.exitCode == 0 || !strings.Contains(refused.stderr, "requires the reviewed original configuration commit") {
		t.Fatalf("restoration verification accepted the cutover declarations: %s", refused.stderr)
	}
	requireMigrationPaused(t, original, "legacy")
	retainCLIPauseAfterRestorationVerification(t, caller)
	refused = caller.run(t, "verify")
	if refused.exitCode == 0 || !strings.Contains(refused.stderr, "cannot verify a migration in phase restored-verified") {
		t.Fatalf("forward verification accepted an inverse terminal checkpoint: %s", refused.stderr)
	}
	requireMigrationPaused(t, original, "legacy")
	requireSuccess(t, caller.run(t, "verify-restored"), "migrate-state", "verify-restored")
	apply := tofuArgs("legacy", "apply", "-auto-approve", "-input=false")
	requireSuccess(t, runConfigsctl(t, original, apply...), apply...)
	if fixtureGit(t, original, "rev-parse", "HEAD") != migration.manifest.RestoreCommit || fixtureGit(t, migration.tree, "rev-parse", "HEAD") != migration.manifest.Commit {
		t.Fatal("recovery changed a reviewed configuration revision")
	}
	requireNoFile(t, filepath.Join(original.workspaces, "application"))
	requireNoFile(t, filepath.Join(original.root, retainedMigrationMirror))
	requireEncryptedMigrationFiles(t, migration.directory)
	if fixtureGit(t, original, "status", "--porcelain") != "" || fixtureGit(t, migration.tree, "status", "--porcelain") != "" {
		t.Fatal("recovery changed a reviewed configuration checkout")
	}
}

func TestMigrationCLIVerifyRestoredChecksTheNewDestinationBackend(t *testing.T) {
	migration, original := newSeparateCheckoutMigration(t)
	for _, operation := range []string{"prepare", "execute", "restore"} {
		requireSuccess(t, migration.run(t, operation), "migrate-state", operation)
	}
	caller := migration
	caller.tree = original
	retainCLIPauseAfterRestorationVerification(t, caller)
	command := exec.CommandContext(t.Context(), "tofu", "apply", "-auto-approve", "-input=false", "-no-color")
	command.Dir = filepath.Join(migration.tree.workspaces, "application")
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + original.home,
		"TF_VAR_vault_tofu_state_passphrase=" + migrationFixtureKey,
		"TF_VAR_vault_migration_fixture=" + migrationFixtureValue,
	}
	if err := command.Run(); err != nil {
		t.Fatalf("create an unrelated native destination instance: %v", err)
	}
	updated := inspectCLIWorkspace(t, migration.tree, "application", filepath.Join(migration.private, "external-update"))
	if updated.Before.Records[applicationInstance] == "" {
		t.Fatal("the external native apply did not create a destination instance")
	}
	refused := caller.run(t, "verify-restored")
	if refused.exitCode == 0 || !strings.Contains(refused.stderr, "application differs from its restoration checkpoint") {
		t.Fatalf("restoration verification skipped the new destination backend: %s", refused.stderr)
	}
	requireMigrationPaused(t, original, "legacy")
	actual := inspectCLIWorkspace(t, migration.tree, "application", filepath.Join(migration.private, "refused-update"))
	if actual.Before.Content != updated.Before.Content || actual.Before.Serial != updated.Before.Serial {
		t.Fatal("refused restoration verification changed the unrelated destination update")
	}
	requireEncryptedMigrationFiles(t, migration.directory)
}

func TestMigrationCLIRequiresAnExplicitConfigurationDirectory(t *testing.T) {
	migration := newCLIMigration(t)
	for _, directory := range []string{"", ".", "relative/checkout"} {
		migration.manifest.ConfigurationDirectory = directory
		migration.writeManifest(t)
		refused := migration.run(t, "prepare")
		if refused.exitCode == 0 || !strings.Contains(refused.stderr, "requires configuration_directory as a clean absolute path") {
			t.Fatalf("preparation accepted a missing or relative configuration directory: %s", refused.stderr)
		}
		requireNoFile(t, filepath.Join(migration.directory, "journal.json"))
	}
}
