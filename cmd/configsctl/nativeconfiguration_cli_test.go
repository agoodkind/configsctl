//go:build unix

package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installNativeConfiguration(t *testing.T, fixture explicitFixture) {
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
		writeFixtureFile(t, filepath.Join(fixture.tree.root, destination), string(body))
	}
}

func TestTofuUsesSelectedNativeGuestOwnership(t *testing.T) {
	fixture := newDirectFixture(t, explicitSettings)
	installNativeConfiguration(t, fixture)
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
	installNativeConfiguration(t, fixture)
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
