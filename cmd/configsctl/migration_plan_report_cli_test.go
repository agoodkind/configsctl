//go:build unix

package main_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type cliPlanReport struct {
	Version   int    `json:"version"`
	Workspace string `json:"workspace"`
	Status    string `json:"status"`
	Changes   []struct {
		Address string   `json:"address"`
		Actions []string `json:"actions"`
		Paths   []string `json:"changed_attribute_paths"`
		Digest  string   `json:"change_digest"`
	} `json:"managed_changes"`
	Diagnostics []struct {
		Severity string `json:"severity"`
		Category string `json:"category"`
		Digest   string `json:"summary_digest"`
		Filename string `json:"filename"`
		Line     int    `json:"line"`
	} `json:"diagnostics"`
}

func readCLIPlanReport(t *testing.T, migration cliMigration, filename string) cliPlanReport {
	t.Helper()
	path := filepath.Join(migration.directory, "reports", filename)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the native plan metadata lacks private file permissions: %v", err)
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || parent.Mode().Perm() != 0o700 {
		t.Fatalf("the native plan metadata lacks a private directory: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "synthetic_required_diagnostic_value_never_print") {
		t.Fatal("the plan metadata exposed a private native diagnostic argument")
	}
	var report cliPlanReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Version != 1 {
		t.Fatal("the native plan metadata omitted its format version")
	}
	return report
}

func TestMigrationPreflightReportsEveryManagedChangeWithoutValues(t *testing.T) {
	migration := newCLIMigration(t)
	updatePreflightFixture(t, &migration, migrationFixtureFile(t, "preflight-metadata.tf"))
	requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
	before := readPreflightSource(t, migration)
	refused := migration.run(t, "preflight")
	if refused.exitCode == 0 || !strings.Contains(refused.stderr, "would change managed resource") {
		t.Fatal("preflight accepted the native managed changes")
	}
	report := readCLIPlanReport(t, migration, "preflight-0001.json")
	if report.Workspace != "application" || report.Status != "managed_changes" || len(report.Changes) != 3 {
		t.Fatal("the public preflight omitted managed changes from its metadata report")
	}
	for index, expected := range []struct{ address, action, path string }{
		{"terraform_data.alpha", "create", "/input"},
		{"terraform_data.beta", "create", "/triggers_replace"},
		{"terraform_data.renamed", "update", "/input/key-sha256:" + cliMetadataDigest(migrationFixtureValue) + "/0"},
	} {
		change := report.Changes[index]
		if change.Address != expected.address || !slices.Equal(change.Actions, []string{expected.action}) || !slices.Contains(change.Paths, expected.path) {
			t.Fatalf("the native change metadata omitted its address, action, or attribute path for %s", expected.address)
		}
		decoded, err := hex.DecodeString(change.Digest)
		if err != nil || len(decoded) != sha256.Size {
			t.Fatal("the native change metadata omitted its complete SHA256 proof")
		}
	}
	passed := readCLIPlanReport(t, migration, "preflight-0000.json")
	if passed.Workspace != "legacy" || passed.Status != "passed" || len(passed.Changes) != 0 {
		t.Fatal("the public preflight did not persist the unchanged workspace result")
	}
	requirePreflightUnchanged(t, migration, before)
}

func cliMetadataDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestMigrationPreflightReportsOnlySafeNativeDiagnosticMetadata(t *testing.T) {
	for _, fixture := range []struct{ file, category, summary string }{
		{"preflight-required-variable.tf", "required_variable", "No value for required variable"},
		{"preflight-sensitive-diagnostic.tf", "unclassified_native_diagnostic", "Resource precondition failed"},
	} {
		t.Run(fixture.category, func(t *testing.T) {
			migration := newCLIMigration(t)
			updatePreflightFixture(t, &migration, migrationFixtureFile(t, fixture.file))
			requireSuccess(t, migration.run(t, "prepare"), "migrate-state", "prepare")
			before := readPreflightSource(t, migration)
			refused := migration.run(t, "preflight")
			if refused.exitCode == 0 {
				t.Fatal("preflight accepted the native plan failure")
			}
			if strings.Contains(refused.stdout+refused.stderr, "synthetic_required_diagnostic_value_never_print") {
				t.Fatal("the public command exposed a private diagnostic argument")
			}
			report := readCLIPlanReport(t, migration, "preflight-0001.json")
			if report.Status != "native_plan_failed" || len(report.Diagnostics) != 1 {
				t.Fatal("the public preflight omitted the native plan failure metadata")
			}
			diagnostic := report.Diagnostics[0]
			if diagnostic.Category != fixture.category || diagnostic.Severity != "error" || diagnostic.Digest != cliMetadataDigest(fixture.summary) || diagnostic.Filename != "main.tf" || diagnostic.Line < 1 {
				t.Fatalf("the native diagnostic metadata omitted its safe category or configuration location: %+v", diagnostic)
			}
			requirePreflightUnchanged(t, migration, before)
		})
	}
}
