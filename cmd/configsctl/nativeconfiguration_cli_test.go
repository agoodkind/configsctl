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

func TestNativeSelectorCannotBeOverriddenByAutomaticVariables(t *testing.T) {
	fixture := newDirectFixture(t, explicitSettings)
	installNativeConfiguration(t, fixture.tree)
	writeFixtureFile(t, filepath.Join(fixture.tree.root, "native", "workspace.auto.tfvars.json"), nativeFixtureFile(t, "selector-override.auto.tfvars.json"))
	writeFixtureFile(t, filepath.Join(fixture.tree.root, "native", "unrelated", "main.tf"), "invalid unselected configuration")
	result := fixture.apply(t, "-auto-approve")
	requireSuccess(t, result)
	if !strings.Contains(result.stderr, "hosts=[guest-alpha]") {
		t.Fatal("automatic variable input replaced the selected native lock ownership")
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

func nativeFixtureFile(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "nativeconfiguration", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestNativeJSONDiagnosticRedactsDeclaredSecrets(t *testing.T) {
	fixture := newDirectFixture(t, explicitSettings)
	installNativeConfiguration(t, fixture.tree)
	root := filepath.Join(fixture.tree.workspaces, "suburban", "tack")
	writeFixtureFile(t, filepath.Join(root, "diagnostic.tf"), nativeFixtureFile(t, "diagnostic.tf"))
	diagnostic := filepath.Join(t.TempDir(), "events.json")
	if err := os.WriteFile(diagnostic, []byte("previous diagnostics\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := fixture.apply(t, "-auto-approve", "-json-into="+diagnostic)
	requireSuccess(t, result)
	body, err := os.ReadFile(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.stdout+result.stderr+string(body), vaultAlphaValue) {
		t.Fatal("the console or JSON diagnostic disclosed a declared secret")
	}
	if !strings.Contains(string(body), "<redacted:TF_VAR_vault_alpha>") {
		t.Fatal("the JSON diagnostic did not use the registered redactor")
	}
	info, err := os.Stat(diagnostic)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("the redacted JSON diagnostic did not replace the prior file with mode 0600")
	}
}

func TestNativeSavedPlanPreservesEphemeralCLIInputs(t *testing.T) {
	for _, option := range []string{"-var", "-var-file"} {
		t.Run(option, func(t *testing.T) {
			fixture := newDirectFixture(t, explicitSettings)
			installNativeConfiguration(t, fixture.tree)
			root := filepath.Join(fixture.tree.workspaces, "suburban", "tack")
			writeFixtureFile(t, filepath.Join(root, "main.tf"), nativeFixtureFile(t, "ephemeral-root.tf"))
			writeFixtureFile(t, filepath.Join(root, "item/main.tf"), nativeFixtureFile(t, "ephemeral-item.tf"))
			writeFixtureFile(t, filepath.Join(root, "item/ephemeral-session.sh"), nativeFixtureFile(t, "ephemeral-session.sh"))
			requireSuccess(t, fixture.mutate(t, "plan", "-input=false", "-var=session=planned-session", "-out=session.plan"))
			arguments := []string{"apply", "-input=false"}
			expected := "applied-session-cli"
			if option == "-var" {
				arguments = append(arguments, "-var=session="+expected)
			} else {
				expected = "applied-session-file"
				variables := filepath.Join(t.TempDir(), "session.tfvars")
				writeFixtureFile(t, variables, nativeFixtureFile(t, "apply-session.tfvars"))
				arguments = append(arguments, "-var-file="+variables)
			}
			arguments = append(arguments, "session.plan")
			requireSuccess(t, fixture.mutate(t, arguments...))
			actual, err := os.ReadFile(filepath.Join(root, "item/session-result.txt"))
			if err != nil || strings.TrimSpace(string(actual)) != expected {
				t.Fatalf("saved-plan apply did not receive its resupplied ephemeral value: %v", err)
			}
		})
	}
}
