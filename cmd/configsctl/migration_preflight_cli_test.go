//go:build unix

package main_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func updatePreflightFixture(t *testing.T, migration *cliMigration, configuration string, paths ...string) {
	t.Helper()
	writeFixtureFile(t, filepath.Join(migration.tree.workspaces, "application", "main.tf"), configuration)
	tracked := append([]string{"workspaces/application/main.tf"}, paths...)
	migration.manifest.Commit = commitMigrationFixture(t, migration.tree, tracked...)
	migration.writeManifest(t)
}

func requirePreflightCleanup(t *testing.T, migration cliMigration) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(migration.directory, "preflight-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("preflight retained its copied checkout or plaintext state: %v, %v", files, err)
	}
	requireEncryptedMigrationFiles(t, migration.directory)
}

func TestMigrationPreflightRefusesManagedChangesBeforeBackendWrites(t *testing.T) {
	for _, operation := range []string{"execute", "resume"} {
		t.Run(operation, func(t *testing.T) {
			migration := newCLIMigration(t)
			updatePreflightFixture(t, &migration, migrationFixtureFile(t, "preflight-change.tf"))
			requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
			source := filepath.Join(migration.tree.workspaces, "legacy", "runtime.tfstate")
			before, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			refused := migration.run(t, operation)
			if refused.exitCode == 0 || !strings.Contains(refused.stderr, "would change managed resource terraform_data.unplanned") {
				t.Fatalf("preflight accepted an unplanned managed change: %s", refused.stderr)
			}
			after, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed preflight changed the original backend state")
			}
			requireNoFile(t, filepath.Join(migration.tree.workspaces, "application", "runtime.tfstate"))
			requirePreflightCleanup(t, migration)
		})
	}
}

func TestMigrationPreflightCopiesOnlyReviewedGitFiles(t *testing.T) {
	migration := newCLIMigration(t)
	ignored := migrationFixtureFile(t, "ignore.txt") + "\n.env\n"
	writeFixtureFile(t, filepath.Join(migration.tree.root, ".gitignore"), ignored)
	updatePreflightFixture(t, &migration, migrationFixtureFile(t, "preflight-private.tf"), ".gitignore")
	for _, path := range []string{".env", filepath.Join(".terraform", "private-cache")} {
		writeFixtureFile(t, filepath.Join(migration.tree.workspaces, "application", path), migrationFixtureValue)
	}
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	requireSuccess(t, migration.run(t, "execute"), "migrate-state", "execute")
	if migrationCLIStateIdentity(t, migration.tree, "application", applicationInstance) != migration.identity {
		t.Fatal("the reviewed-file preflight changed the prepared instance")
	}
	requirePreflightCleanup(t, migration)
}

func TestMigrationPreflightRefusesExternalArchiveSymlinks(t *testing.T) {
	migration := newCLIMigration(t)
	outside := filepath.Join(migration.private, "outside")
	writeFixtureFile(t, outside, migrationFixtureValue)
	link := filepath.Join(migration.tree.root, "outside-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	migration.manifest.Commit = commitMigrationFixture(t, migration.tree, "outside-link")
	migration.writeManifest(t)
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	refused := migration.run(t, "execute")
	if refused.exitCode == 0 || !strings.Contains(refused.stderr, "symbolic link outside the copied repository") {
		t.Fatalf("preflight accepted an external archive link: %s", refused.stderr)
	}
	requireNoFile(t, filepath.Join(migration.tree.workspaces, "application", "runtime.tfstate"))
	requirePreflightCleanup(t, migration)
}

func TestMigrationPreflightCleansInterruptedFilesOnlyUnderExecutorOwnership(t *testing.T) {
	migration := newCLIMigration(t)
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	var interrupted []string
	for _, prefix := range []string{"preflight-", "restore-check-"} {
		plaintext := filepath.Join(migration.directory, prefix+"interrupted", "prepared.tfstate")
		writeFixtureFile(t, plaintext, migrationFixtureValue)
		interrupted = append(interrupted, plaintext)
	}
	requireCLIExecutorLock(t, migration)
	for _, plaintext := range interrupted {
		requireFile(t, plaintext)
	}
	requireSuccess(t, migration.run(t, "execute"), "migrate-state", "execute")
	for _, plaintext := range interrupted {
		requireNoFile(t, filepath.Dir(plaintext))
	}
	requirePreflightCleanup(t, migration)
}
