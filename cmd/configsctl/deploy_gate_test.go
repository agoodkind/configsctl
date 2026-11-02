package main_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const gatedWorkspace = `
module "base" {
  source   = "./item"
  for_each = toset(["guest_alpha"])
  name     = each.key
  healthy  = %t
}
`

const gatedItemModule = `
variable "name" {
  type = string
}

variable "healthy" {
  type = bool
}

resource "terraform_data" "item" {
  input = var.name

  lifecycle {
    precondition {
      condition     = var.healthy
      error_message = "fixture precondition failed for ${var.name}"
    }
  }
}
`

const gatedInventory = `all:
  hosts:
    guest-alpha:
      ansible_connection: local
      service_name: guest_alpha
    plain-host:
      ansible_connection: local
`

const gatedSettings = `  guest_module: module.base
  lock_targets:
    workspaces/guest:
      guest_hosts:
        site/101: guest-alpha
`

const gatedPlaybook = `- name: Write the play marker
  hosts: all
  gather_facts: false
  tasks:
    - name: Write the play marker
      ansible.builtin.copy:
        content: "played\n"
        dest: "{{ playbook_dir }}/played"
        mode: "0600"
`

const (
	gatedPlaybookName   = "deploy-fixture"
	pendingRefusal      = "guest guest_alpha (host guest-alpha) has 1 pending resource changes"
	failedRefusal       = "the plan of guest guest_alpha (host guest-alpha) failed"
	planLogNotice       = "The plan output is in "
	planStreamLines     = 600000
	planStreamsVariable = "CONFIGSCTL_TEST_PLAN_STREAMS"
	stdoutMarker        = "p"
	stderrMarker        = "b"
	slogErrorLevel      = " ERROR "
	eventLineLimit      = 300
	raceReportStart     = "WARNING: DATA RACE"
	raceReportEnd       = "=================="
	panicMessage        = "panic"
	planFailedEvent     = "deploy.gate.plan_failed"
)

var (
	planStdoutLine = vaultAlphaValue[:len(vaultAlphaValue)-1]
	planStderrLine = vaultBetaValue[:len(vaultBetaValue)-1]
)

func requireAnsiblePlaybook(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Skip("ansible-playbook is not installed")
	}
}

func newGatedTree(t *testing.T, healthy bool) configsTree {
	t.Helper()
	requireTofu(t)
	requireAnsiblePlaybook(t)
	tree := newConfigsTree(t)
	writeFixtureFile(t, filepath.Join(tree.root, "configsctl.yml"), settingsContent+gatedSettings)
	guest := filepath.Join(tree.workspaces, "guest")
	writeFixtureFile(t, filepath.Join(guest, "backend.tf"), fmt.Sprintf(localBackend, "guest.tfstate"))
	writeFixtureFile(t, filepath.Join(guest, "main.tf"), fmt.Sprintf(gatedWorkspace, healthy))
	writeFixtureFile(t, filepath.Join(guest, "item", "main.tf"), gatedItemModule)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "ansible.cfg"), lockedAnsibleConfig)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "inventory", "hosts.yml"), gatedInventory)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "playbooks", gatedPlaybookName+".yml"), gatedPlaybook)

	initArgs := tofuArgs("guest", "init", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, initArgs...), initArgs...)
	return tree
}

func playMarker(tree configsTree) string {
	return filepath.Join(tree.root, "ansible", "playbooks", "played")
}

func requireRefusal(t *testing.T, tree configsTree, result commandResult, fragments ...string) {
	t.Helper()
	if result.exitCode == 0 {
		t.Fatalf("Deploy succeeded. The test expected the plan gate to refuse it.\nstderr: %s", result.stderr)
	}
	for _, fragment := range fragments {
		if !strings.Contains(result.stderr, fragment) {
			t.Fatalf("stderr = %q, want it to mention %q", result.stderr, fragment)
		}
	}
	requireNoFile(t, playMarker(tree))
}

func TestDeployRunsThePlayForAGuestWithoutPendingChanges(t *testing.T) {
	tree := newGatedTree(t, true)
	applyArgs := tofuArgs("guest", "apply", "-auto-approve", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, applyArgs...), applyArgs...)

	result := runConfigsctl(t, tree, "deploy", gatedPlaybookName)
	requireSuccess(t, result, "deploy", gatedPlaybookName)
	if !strings.Contains(result.stderr, "deploy.gate.passed guest=guest_alpha") {
		t.Fatalf("stderr = %q, want the gate to pass guest_alpha", result.stderr)
	}
	requireFile(t, playMarker(tree))
}

func TestDeployRefusesAGuestWithPendingChanges(t *testing.T) {
	tree := newGatedTree(t, true)

	result := runConfigsctl(t, tree, "deploy", gatedPlaybookName)
	requireRefusal(t, tree, result, pendingRefusal, "terraform_data.item")
}

func TestDeployRefusesAGuestWithAFailedPlan(t *testing.T) {
	tree := newGatedTree(t, false)

	result := runConfigsctl(t, tree, "deploy", gatedPlaybookName)
	requireRefusal(t, tree, result, failedRefusal, "fixture precondition failed for guest_alpha")
}

func TestDeployLogsEveryPlanLineFromBothStreams(t *testing.T) {
	tree := newGatedTree(t, true)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve the test binary: %v", err)
	}
	binDir := t.TempDir()
	if err := os.Symlink(testBinary, filepath.Join(binDir, "tofu")); err != nil {
		t.Fatalf("link the test binary as tofu: %v", err)
	}
	path := binDir + string(os.PathListSeparator) + os.Getenv("PATH")
	env := []string{planStreamsVariable + "=1"}

	result := runConfigsctlWithEnv(t, tree, path, env, "deploy", gatedPlaybookName)
	requireRefusal(t, tree, result, failedRefusal)
	events := eventLines(result.stderr)
	if strings.Contains(result.stderr, panicMessage) {
		t.Fatalf("configsctl reported a panic\nevents:\n%s", events)
	}
	log := readPlanLog(t, result.stderr)
	wantBytes := planStreamLines * (len(planStdoutLine) + len(planStderrLine) + 2)
	if len(log) != wantBytes {
		t.Fatalf("the plan log contains %d bytes, want %d\nevents:\n%s", len(log), wantBytes, events)
	}
	for _, marker := range []string{stdoutMarker, stderrMarker} {
		if count := strings.Count(log, marker); count != planStreamLines {
			t.Fatalf("the plan log contains %d copies of %q, want %d\nevents:\n%s",
				count, marker, planStreamLines, events)
		}
	}
	failure := planFailureEvent(t, result.stderr)
	if count := strings.Count(failure, planStderrLine); count != planStreamLines {
		t.Fatalf("the plan failure event reports %d standard error lines, want %d\nevents:\n%s",
			count, planStreamLines, events)
	}
	if strings.Contains(result.stderr, raceReportStart) {
		t.Fatalf("configsctl reported a data race\nevents:\n%s", events)
	}
}

func writePlanStreams() int {
	var writers sync.WaitGroup
	streams := map[*os.File]string{os.Stdout: planStdoutLine, os.Stderr: planStderrLine}
	for stream, line := range streams {
		writers.Go(func() {
			_, _ = stream.WriteString(strings.Repeat(line+"\n", planStreamLines))
		})
	}
	writers.Wait()
	return 1
}

func planFailureEvent(t *testing.T, stderr string) string {
	t.Helper()
	for line := range strings.SplitSeq(stderr, "\n") {
		if strings.Contains(line, planFailedEvent) {
			return line
		}
	}
	t.Fatalf("stderr does not contain the %s event\nevents:\n%s", planFailedEvent, eventLines(stderr))
	return ""
}

func readPlanLog(t *testing.T, stderr string) string {
	t.Helper()
	for line := range strings.SplitSeq(stderr, "\n") {
		path, found := strings.CutPrefix(line, planLogNotice)
		if !found {
			continue
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read the plan log: %v", err)
		}
		return string(contents)
	}
	t.Fatalf("stderr does not name the plan log\nevents:\n%s", eventLines(stderr))
	return ""
}

func eventLines(text string) string {
	var lines []string
	inRaceReport := false
	for line := range strings.SplitSeq(text, "\n") {
		if line == raceReportStart {
			inRaceReport = true
		}
		if inRaceReport || strings.Contains(line, slogErrorLevel) {
			lines = append(lines, line[:min(len(line), eventLineLimit)])
		}
		if line == raceReportEnd {
			inRaceReport = false
		}
	}
	return strings.Join(lines, "\n")
}

func TestDeployRunsThePlayForAHostOutsideGuestHosts(t *testing.T) {
	tree := newGatedTree(t, true)

	result := runConfigsctl(t, tree, "deploy", gatedPlaybookName, "--limit", "plain-host")
	requireSuccess(t, result, "deploy", gatedPlaybookName, "--limit", "plain-host")
	if !strings.Contains(result.stderr, "deploy.gate.hosts_skipped hosts=[plain-host]") {
		t.Fatalf("stderr = %q, want the gate to skip plain-host", result.stderr)
	}
	if strings.Contains(result.stderr, "deploy.gate.plan") {
		t.Fatalf("stderr = %q, want no plan for plain-host", result.stderr)
	}
	requireFile(t, playMarker(tree))
}

func TestDeployInCheckModeRunsNoPlan(t *testing.T) {
	tree := newGatedTree(t, true)

	result := runConfigsctl(t, tree, "deploy", gatedPlaybookName, "--check")
	requireSuccess(t, result, "deploy", gatedPlaybookName, "--check")
	if strings.Contains(result.stderr, "deploy.gate") {
		t.Fatalf("stderr = %q, want no plan gate in check mode", result.stderr)
	}
}
