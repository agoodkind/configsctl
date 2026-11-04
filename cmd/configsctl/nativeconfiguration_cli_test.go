//go:build unix

package main_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installNativeConfiguration(t *testing.T, tree configsTree) {
	t.Helper()
	for source, destination := range map[string]string{
		"root.tf":      "native/main.tf",
		"targets.tf":   "native/suburban/tack/main.tf",
		"settings.yml": settingsName,
	} {
		body, err := os.ReadFile(filepath.Join("testdata", "nativeconfiguration", source))
		if err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, filepath.Join(tree.root, destination), string(body))
	}
}

func TestTofuUsesSelectedNativeGuestOwnership(t *testing.T) {
	fixture := newDirectFixture(t, explicitSettings)
	installNativeConfiguration(t, fixture.tree)
	writeFixtureFile(t, filepath.Join(fixture.tree.root, "native", "unrelated", "main.tf"), "invalid unrelated feature")
	writeFixtureFile(t, filepath.Join(fixture.tree.root, "native", ".terraform", "environment"), "unrelated")
	result := fixture.apply(t, "-auto-approve")
	requireSuccess(t, result)
	if !strings.Contains(result.stderr, "hosts=[guest-alpha]") {
		t.Fatalf("the CLI did not select the native guest owner: %s", result.stderr)
	}
	if strings.Contains(result.stderr, "hosts=[hv_alpha]") {
		t.Fatalf("the CLI locked an unrelated hypervisor: %s", result.stderr)
	}
}

func TestTofuRefusesMissingSelectedNativeInputs(t *testing.T) {
	fixture := newDirectFixture(t, explicitSettings)
	installNativeConfiguration(t, fixture.tree)
	path := filepath.Join(fixture.tree.root, "native", "suburban", "tack", "main.tf")
	if err := os.Rename(path, path+".disabled"); err != nil {
		t.Fatal(err)
	}
	result := fixture.apply(t, "-auto-approve")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "evaluate native configuration") {
		t.Fatalf("the CLI did not refuse the missing native input: %s", result.stderr)
	}
	if strings.Contains(result.stderr, "hostlock.acquired") {
		t.Fatalf("the CLI acquired locks before validating native inputs: %s", result.stderr)
	}
}

func TestNativeEvaluationPreservesTofuJSONOutput(t *testing.T) {
	fixture := newDirectFixture(t, explicitSettings)
	installNativeConfiguration(t, fixture.tree)
	requireSuccess(t, fixture.apply(t, "-auto-approve"))
	result := fixture.mutate(t, "show", "-json")
	requireSuccess(t, result)
	var document struct {
		FormatVersion string `json:"format_version"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &document); err != nil {
		t.Fatalf("native diagnostics changed OpenTofu JSON stdout: %v", err)
	}
	if document.FormatVersion == "" {
		t.Fatal("the CLI did not print the actual OpenTofu JSON document")
	}
}

func TestNativeApplyPreservesRefreshOnlyAndDiagnosticOptions(t *testing.T) {
	for name, options := range map[string][]string{
		"refresh only": {"-refresh-only", "-lock=true", "-compact-warnings", "-concise", "-consolidate-errors", "-consolidate-warnings=false", "-deprecation=module:none"},
		"json":         {"-json", "-lock=true"},
		"json file":    {"-json-into=events.json", "-lock=true"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newDirectFixture(t, explicitSettings)
			installNativeConfiguration(t, fixture.tree)
			arguments := append([]string{"-auto-approve"}, options...)
			requireSuccess(t, fixture.apply(t, arguments...))
			state := fixture.mutate(t, "state", "list")
			requireSuccess(t, state)
			if name == "refresh only" {
				if strings.TrimSpace(state.stdout) != "" {
					t.Fatal("refresh-only created a configured resource")
				}
			} else if !strings.Contains(state.stdout, `module.base["guest_alpha"].terraform_data.item`) {
				t.Fatal("the diagnostic apply did not create its configured resource")
			}
			if name == "json file" {
				requireFile(t, filepath.Join(fixture.tree.workspaces, "suburban/tack/events.json"))
			}
		})
	}
}
