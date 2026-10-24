//go:build unix

package main_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"goodkind.io/configsctl/internal/statemigration"
)

const (
	migrationFixtureValue = "synthetic-cli-migration-value-never-print-195b"
	migrationFixtureKey   = "synthetic-cli-migration-passphrase-never-print-91d2"
	legacyInstance        = "terraform_data.original"
	applicationInstance   = "terraform_data.renamed"
)

type cliMigration struct {
	tree      configsTree
	private   string
	directory string
	manifest  statemigration.Manifest
	file      string
	identity  string
}

func migrationFixtureFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "migration", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func fixtureGit(t *testing.T, tree configsTree, args ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", args...)
	command.Dir = tree.root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("the temporary Git fixture failed: %v", err)
	}
	return strings.TrimSpace(string(output))
}

func commitMigrationFixture(t *testing.T, tree configsTree, paths ...string) string {
	t.Helper()
	fixtureGit(t, tree, append([]string{"add", "--"}, paths...)...)
	fixtureGit(t, tree, "-c", "user.name=Configsctl Test", "-c", "user.email=configsctl-test@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "-m", "Save synthetic migration configuration")
	return fixtureGit(t, tree, "rev-parse", "HEAD")
}

func inspectCLIWorkspace(t *testing.T, tree configsTree, selector, directory string) statemigration.WorkspaceSpec {
	t.Helper()
	args := []string{"migrate-state", "inspect", selector, directory}
	result := runConfigsctl(t, tree, args...)
	requireSuccess(t, result, args...)
	requireMigrationPrivateOutput(t, result)
	data, err := os.ReadFile(filepath.Join(directory, "inventory.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec statemigration.WorkspaceSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

func migrationCLIStateIdentity(t *testing.T, tree configsTree, selector, address string) string {
	t.Helper()
	args := tofuArgs(selector, "state", "show", "-no-color", address)
	result := runConfigsctl(t, tree, args...)
	requireSuccess(t, result, args...)
	requireMigrationPrivateOutput(t, result)
	for _, line := range strings.Split(result.stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "id" && fields[1] == "=" {
			return strings.Trim(fields[2], `"`)
		}
	}
	t.Fatal("the synthetic instance did not expose its ID through state show")
	return ""
}

func newCLIMigration(t *testing.T) cliMigration {
	t.Helper()
	tree := newConfigsTree(t)
	values := "vault_tofu_state_passphrase: " + migrationFixtureKey + "\nvault_migration_fixture: " + migrationFixtureValue + "\n"
	seed := runMutationInput(t, tree, values, "set-secrets")
	requireSuccess(t, seed, "set-secrets")
	requireMigrationPrivateOutput(t, seed)
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), migrationFixtureFile(t, "settings.yml"))
	writeFixtureFile(t, filepath.Join(tree.root, ".gitignore"), migrationFixtureFile(t, "ignore.txt"))
	for _, selector := range []string{"legacy", "application"} {
		writeFixtureFile(t, filepath.Join(tree.workspaces, selector, "backend.tf"), migrationFixtureFile(t, "backend.tf"))
	}
	writeFixtureFile(t, filepath.Join(tree.workspaces, "legacy", "main.tf"), migrationFixtureFile(t, "source.tf"))
	writeFixtureFile(t, filepath.Join(tree.workspaces, "application", "main.tf"), migrationFixtureFile(t, "destination.tf"))
	fixtureGit(t, tree, "init", "--initial-branch=main")
	recovery := commitMigrationFixture(t, tree, ".gitignore", settingsName, "workspaces", "ansible")
	for _, selector := range []string{"legacy", "application"} {
		args := tofuArgs(selector, "init", "-input=false")
		requireSuccess(t, runConfigsctl(t, tree, args...), args...)
	}
	apply := tofuArgs("legacy", "apply", "-auto-approve", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, apply...), apply...)
	identity := migrationCLIStateIdentity(t, tree, "legacy", legacyInstance)
	private := t.TempDir()
	before := inspectCLIWorkspace(t, tree, "legacy", filepath.Join(private, "source-inspection"))
	destination := inspectCLIWorkspace(t, tree, "application", filepath.Join(private, "destination-inspection"))
	writeFixtureFile(t, filepath.Join(tree.workspaces, "legacy", "main.tf"), migrationFixtureFile(t, "retired.tf"))
	cutover := commitMigrationFixture(t, tree, "workspaces/legacy/main.tf")
	manifest := statemigration.Manifest{
		Version: 1, Commit: cutover, RestoreCommit: recovery, ConfigurationDirectory: tree.root,
		Workspaces: []statemigration.WorkspaceSpec{before, destination},
		Moves:      []statemigration.Transfer{{Source: "legacy", Address: legacyInstance, Destination: "application", NewAddress: applicationInstance}},
	}
	migration := cliMigration{tree: tree, private: private, directory: filepath.Join(private, "transfer"), manifest: manifest, file: filepath.Join(private, "manifest.json"), identity: identity}
	migration.writeManifest(t)
	return migration
}

func (m cliMigration) writeManifest(t *testing.T) {
	t.Helper()
	data, err := json.Marshal(m.manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, m.file, string(data))
}

func requireMigrationPrivateOutput(t *testing.T, result commandResult) {
	t.Helper()
	for _, marker := range []string{migrationFixtureValue, migrationFixtureKey} {
		if strings.Contains(result.stdout+result.stderr, marker) {
			t.Fatal("migration output contains a synthetic secret value")
		}
	}
}

func requireEncryptedMigrationFiles(t *testing.T, directory string) {
	t.Helper()
	if err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(entry.Name(), ".tfstate") {
			info, err := entry.Info()
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatal("retained native snapshot and backend metadata files must have mode 0600")
			}
		}
		if bytes.Contains(data, []byte(migrationFixtureValue)) || bytes.Contains(data, []byte(migrationFixtureKey)) {
			t.Fatal("retained migration files contain a synthetic plaintext secret")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (m cliMigration) run(t *testing.T, operation string) commandResult {
	t.Helper()
	result := runConfigsctl(t, m.tree, "migrate-state", operation, m.file, m.directory)
	requireMigrationPrivateOutput(t, result)
	return result
}

func TestMigrationCLIInspectRetainsOnlyEncryptedStateAndMetadata(t *testing.T) {
	migration := newCLIMigration(t)
	directory := filepath.Join(migration.private, "source-inspection")
	metadata, err := os.Stat(filepath.Join(directory, "inventory.json"))
	if err != nil || metadata.Mode().Perm() != 0o600 {
		t.Fatalf("the inspection metadata lacks private permissions: %v", err)
	}
	requireNoFile(t, filepath.Join(directory, "transient.tfstate"))
	data, err := os.ReadFile(filepath.Join(directory, "encrypted", "snapshot.tfstate"))
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil || len(envelope["encrypted_data"]) == 0 || len(envelope["resources"]) != 0 {
		t.Fatal("the native inspection snapshot does not contain encrypted state")
	}
	if migration.manifest.Workspaces[0].Before.Records[legacyInstance] == "" {
		t.Fatal("the inspection metadata omitted the real instance proof")
	}
	requireEncryptedMigrationFiles(t, directory)
}

func TestMigrationCLITransfersResumesAndVerifiesNativeIdentity(t *testing.T) {
	migration := newCLIMigration(t)
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	requireCLIExecutorLock(t, migration)
	for _, operation := range []string{"execute", "resume"} {
		requireSuccess(t, migration.run(t, operation), "migrate-state", operation)
	}
	if identity := migrationCLIStateIdentity(t, migration.tree, "application", applicationInstance); identity != migration.identity {
		t.Fatal("the public migration command changed the instance ID")
	}
	blocked := runConfigsctl(t, migration.tree, tofuArgs("application", "apply", "-auto-approve", "-input=false")...)
	if blocked.exitCode == 0 || !strings.Contains(blocked.stderr, "writes remain paused") {
		t.Fatalf("the incomplete migration allowed an ordinary apply: %s", blocked.stderr)
	}
	requireSuccess(t, migration.run(t, "verify"), "migrate-state", "verify")
	after := inspectCLIWorkspace(t, migration.tree, "application", filepath.Join(migration.private, "after-inspection"))
	if after.Before.Records[applicationInstance] != migration.manifest.Workspaces[0].Before.Records[legacyInstance] {
		t.Fatal("the public migration command changed instance or provider-state contents")
	}
	apply := tofuArgs("application", "apply", "-auto-approve", "-input=false")
	requireSuccess(t, runConfigsctl(t, migration.tree, apply...), apply...)
	requireEncryptedMigrationFiles(t, migration.directory)
}

func requireCLIExecutorLock(t *testing.T, migration cliMigration) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(migration.directory, "executor.lock"), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	refused := migration.run(t, "execute")
	if refused.exitCode == 0 || !strings.Contains(refused.stderr, "another process owns this migration executor") {
		t.Fatalf("the public migration command bypassed its actual executor lock: %s", refused.stderr)
	}
}

func TestMigrationCLIRefusesStaleCommitAndDuplicateMapping(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(stringBool(duplicate), func(t *testing.T) {
			migration := newCLIMigration(t)
			fragment := "the retained cutover checkout does not match the reviewed cutover configuration commit"
			if duplicate {
				migration.manifest.Moves = append(migration.manifest.Moves, migration.manifest.Moves[0])
				fragment = "mapped more than once"
			} else {
				migration.manifest.Commit = migration.manifest.RestoreCommit
			}
			migration.writeManifest(t)
			result := migration.run(t, "prepare")
			if result.exitCode == 0 || !strings.Contains(result.stderr, fragment) {
				t.Fatalf("the invalid migration did not refuse before writing state: %s", result.stderr)
			}
			requireNoFile(t, filepath.Join(migration.directory, "journal.json"))
			if identity := migrationCLIStateIdentity(t, migration.tree, "legacy", legacyInstance); identity != migration.identity {
				t.Fatal("a refused migration changed the original instance")
			}
		})
	}
}

func TestMigrationCLIRefusesCheckoutAndSymlinkDirectories(t *testing.T) {
	migration := newCLIMigration(t)
	inside := filepath.Join(migration.tree.root, "private-migration")
	result := runConfigsctl(t, migration.tree, "migrate-state", "inspect", "legacy", inside)
	if result.exitCode == 0 || !strings.Contains(result.stderr, "outside the configuration checkout") {
		t.Fatalf("the inspection accepted a directory inside the checkout: %s", result.stderr)
	}
	requireNoFile(t, inside)
	target := filepath.Join(migration.private, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(migration.private, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	result = runConfigsctl(t, migration.tree, "migrate-state", "inspect", "legacy", link)
	if result.exitCode == 0 {
		t.Fatal("the inspection accepted a symbolic link as its private directory")
	}
	requireMigrationPrivateOutput(t, result)
	requireNoFile(t, filepath.Join(target, "inventory.json"))
	requireNoFile(t, filepath.Join(target, "transient.tfstate"))
}
