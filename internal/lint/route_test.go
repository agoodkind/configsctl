package lint

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// requireOracle fails when python3 or jinja2 is absent, since the lint gate
// cannot enforce a routed form without them and a skipped route test would pass
// while that path is broken. It also moves the test into an empty working
// directory, so a pass proves the embedded oracle runs without any script beside
// the caller.
func requireOracle(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatalf("python3 is required: %v", err)
	}
	if err := exec.Command("python3", "-I", "-c", "import jinja2").Run(); err != nil {
		t.Fatalf("jinja2 is required for python3 in isolated mode: %v", err)
	}
	t.Chdir(t.TempDir())
}

// TestRunRoutesMembershipThroughOracle locks in the enforcement path: a
// membership check on an input variable that the Go engine cannot parse, because
// it sits inside a parenthesized conditional piped into a filter, is routed to
// the jinja2 oracle and returned as a hard violation.
func TestRunRoutesMembershipThroughOracle(t *testing.T) {
	requireOracle(t)

	dir := t.TempDir()
	play := filepath.Join(dir, "play.yml")
	content := "- hosts: localhost\n" +
		"  vars:\n" +
		"    target_group: \"{{ target_hosts }}\"\n" +
		"    guests: >-\n" +
		"      {{\n" +
		"        (groups[target_group] if target_group in groups else [target_group])\n" +
		"        | map('extract', hostvars, 'guest_info') | select('defined') | list\n" +
		"      }}\n"
	if err := os.WriteFile(play, []byte(content), 0o600); err != nil {
		t.Fatalf("write play: %v", err)
	}

	findings, _, err := Run([]string{play})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, finding := range findings {
		if finding.Kind == "membership" && finding.Root == "target_group" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected membership violation on target_group via oracle, got %v", findings)
	}
}

// TestRunSparesRuntimeRootInRoutedForm confirms the oracle path does not
// over-flag: the same unparsed shape over a registered result is spared.
func TestRunSparesRuntimeRootInRoutedForm(t *testing.T) {
	requireOracle(t)

	dir := t.TempDir()
	play := filepath.Join(dir, "play.yml")
	content := "- hosts: localhost\n" +
		"  tasks:\n" +
		"    - command: echo hi\n" +
		"      register: probe\n" +
		"    - debug:\n" +
		"        msg: >-\n" +
		"          {{\n" +
		"            (probe.stdout if probe.stdout in groups else [probe.stdout])\n" +
		"            | map('trim') | list\n" +
		"          }}\n"
	if err := os.WriteFile(play, []byte(content), 0o600); err != nil {
		t.Fatalf("write play: %v", err)
	}

	findings, _, err := Run([]string{play})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, finding := range findings {
		if finding.Root == "probe" {
			t.Fatalf("registered result probe should be spared, got %v", finding)
		}
	}
}
