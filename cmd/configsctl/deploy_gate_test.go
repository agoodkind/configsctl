package main_test

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
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
	gatedPlaybookName = "deploy-fixture"
	pendingRefusal    = "guest guest_alpha (host guest-alpha) has 1 pending resource changes"
	failedRefusal     = "the plan of guest guest_alpha (host guest-alpha) failed"
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
