//go:build unix

package main_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func newPlanInspectionTree(t *testing.T) configsTree {
	t.Helper()
	tree := newConfigsTree(t)
	values := "vault_tofu_state_passphrase: " + migrationFixtureKey + "\nvault_migration_fixture: " + migrationFixtureValue + "\n"
	requireSuccess(t, runMutationInput(t, tree, values, "set-secrets"), "set-secrets")
	settings := migrationFixtureFile(t, "settings.yml")
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), settings)
	writeFixtureFile(t, filepath.Join(tree.workspaces, "legacy", "backend.tf"), migrationFixtureFile(t, "backend.tf"))
	writeFixtureFile(t, filepath.Join(tree.workspaces, "legacy", "main.tf"), migrationFixtureFile(t, "source.tf"))
	applyWorkspace(t, tree, "legacy")
	credential := strings.Replace(readCredentialFixture(t, "credentials.yml"), "workspaces/alpha:", "workspaces/legacy:", 1)
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), settings+credential)
	return tree
}

func TestMigrationPlanInspectionRetainsSafeMetadataWithoutWrites(t *testing.T) {
	for _, fixture := range []struct {
		file    string
		status  string
		changes int
	}{
		{"source.tf", "passed", 0},
		{"preflight-metadata.tf", "managed_changes", 3},
		{"baseline-original-error.tf", "native_plan_failed", 0},
	} {
		t.Run(fixture.status, func(t *testing.T) {
			tree := newPlanInspectionTree(t)
			configuration := strings.Replace(migrationFixtureFile(t, fixture.file), `"renamed"`, `"original"`, 1)
			writeFixtureFile(t, filepath.Join(tree.workspaces, "legacy", "main.tf"), configuration)
			state := filepath.Join(tree.workspaces, "legacy", "runtime.tfstate")
			before := readPlanInspectionBytes(t, state)
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(before, &envelope); err != nil || len(envelope["encrypted_data"]) == 0 {
				t.Fatal("the plan inspection fixture did not create encrypted native state")
			}
			directory := filepath.Join(t.TempDir(), "inspection")
			result := runConfigsctl(t, tree, "migrate-state", "inspect-plan", "legacy", directory)
			if (result.exitCode == 0) != (fixture.status == "passed") {
				t.Fatalf("the strict native plan inspection returned an unexpected result: %s", result.stderr)
			}
			requireMigrationPrivateOutput(t, result)
			report, reportPath := readPlanInspectionReport(t, directory)
			workspace, err := filepath.EvalSymlinks(filepath.Join(tree.workspaces, "legacy"))
			if err != nil {
				t.Fatal(err)
			}
			if report.Workspace != workspace || report.Status != fixture.status || len(report.Changes) != fixture.changes {
				t.Fatal("the inspection omitted the native workspace status or complete managed changes")
			}
			if fixture.status == "managed_changes" {
				for index, expected := range []struct{ address, action string }{
					{"terraform_data.alpha", "create"}, {"terraform_data.beta", "create"}, {"terraform_data.original", "update"},
				} {
					change := report.Changes[index]
					proof, err := hex.DecodeString(change.Digest)
					if change.Address != expected.address || !slices.Equal(change.Actions, []string{expected.action}) || len(change.Paths) == 0 || err != nil || len(proof) != sha256.Size {
						t.Fatal("the inspection omitted a managed address, action, changed path, or complete digest")
					}
				}
			}
			if fixture.status == "native_plan_failed" {
				if len(report.Diagnostics) != 1 || report.Diagnostics[0].Severity != "error" || report.Diagnostics[0].Category != "unclassified_native_diagnostic" || report.Diagnostics[0].Digest != cliMetadataDigest("Resource precondition failed") || report.Diagnostics[0].Filename != "main.tf" || report.Diagnostics[0].Line < 1 {
					t.Fatal("the inspection omitted the native failure's safe diagnostic metadata")
				}
			}
			resolved, err := filepath.EvalSymlinks(reportPath)
			if err != nil || result.stdout != "Private plan metadata: "+resolved+"\n" {
				t.Fatal("the inspection printed content beyond its safe metadata location")
			}
			if !bytes.Equal(before, readPlanInspectionBytes(t, state)) {
				t.Fatal("the plan inspection changed the actual encrypted backend bytes")
			}
			requirePlanInspectionCleanup(t, tree, directory, result)
			retained := readPlanInspectionBytes(t, reportPath)
			reused := runConfigsctl(t, tree, "migrate-state", "inspect-plan", "legacy", directory)
			if reused.exitCode == 0 || !bytes.Equal(retained, readPlanInspectionBytes(t, reportPath)) {
				t.Fatal("the inspection replaced metadata in an existing private directory")
			}
		})
	}
}

func readPlanInspectionReport(t *testing.T, directory string) (cliPlanReport, string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(directory, "reports", "verification-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal("the plan inspection did not retain exactly one safe metadata report")
	}
	report := readCLIPlanReport(t, cliMigration{directory: directory}, filepath.Base(files[0]))
	return report, files[0]
}

func readPlanInspectionBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func requirePlanInspectionCleanup(t *testing.T, tree configsTree, directory string, result commandResult) {
	t.Helper()
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal("the plan inspection directory does not have mode 0700")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != "reports" {
		t.Fatal("the inspection retained raw native plans, events, state, or journal files")
	}
	requireEncryptedMigrationFiles(t, directory)
	if err := filepath.WalkDir(tree.home, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if slices.Contains([]string{"pause.json", "journal.json"}, entry.Name()) {
			t.Fatal("the plan inspection created migration coordination state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"hostlock.acquired", "tofu.credential", "dell.credential", "iDRAC credential", `"type":"planned_change"`} {
		if strings.Contains(result.stdout+result.stderr, fragment) {
			t.Fatal("the plan inspection invoked credential preparation or emitted raw native events")
		}
	}
}

func TestMigrationPlanInspectionRefusesUnsafeSelectionsAndDirectories(t *testing.T) {
	tree := newPlanInspectionTree(t)
	private := t.TempDir()
	inside := filepath.Join(tree.root, "inspection")
	refused := runConfigsctl(t, tree, "migrate-state", "inspect-plan", "legacy", inside)
	if refused.exitCode == 0 || !strings.Contains(refused.stderr, "outside the configuration checkout") {
		t.Fatal("the plan inspection accepted an output directory inside the checkout")
	}
	requireNoFile(t, inside)
	target := filepath.Join(private, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(private, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	refused = runConfigsctl(t, tree, "migrate-state", "inspect-plan", "legacy", link)
	if refused.exitCode == 0 {
		t.Fatal("the plan inspection accepted a symbolic link as its output directory")
	}
	requireNoFile(t, filepath.Join(target, "reports"))
	directory := filepath.Join(private, "invalid-selector")
	refused = runConfigsctl(t, tree, "migrate-state", "inspect-plan", "../legacy", directory)
	if refused.exitCode == 0 || !strings.Contains(refused.stderr, "workspace selector") {
		t.Fatal("the plan inspection accepted a traversing workspace selector")
	}
	requireNoFile(t, directory)
}
