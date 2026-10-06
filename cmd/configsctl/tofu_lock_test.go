package main_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

const lockedWorkspace = `
module "alpha" {
  source = "./item"
  value  = "alpha"
}

module "beta" {
  source = "./item"
  value  = "beta"
}

module "gamma" {
  source = "./item"
  value  = "gamma"
}
`

const lockedItemModule = `
variable "value" {
  type = string
}

resource "terraform_data" "item" {
  input = var.value
}
`

const lockedAnsibleConfig = "[defaults]\ninventory = inventory/hosts.yml\n"

const lockedInventory = `all:
  children:
    proxmox_servers:
      hosts:
        hv_alpha:
          ansible_connection: local
        hv_beta:
          ansible_connection: local
`

const lockTargetsSettings = `  lock_targets:
    workspaces/locked:
      module_hosts:
        module.alpha: hv_alpha
        module.beta: hv_beta
`

func newLockedTree(t *testing.T) configsTree {
	t.Helper()
	tree := newConfigsTree(t)
	writeFixtureFile(t, filepath.Join(tree.root, "configsctl.yml"), settingsContent+lockTargetsSettings)
	locked := filepath.Join(tree.workspaces, "locked")
	writeFixtureFile(t, filepath.Join(locked, "backend.tf"), fmt.Sprintf(localBackend, "locked.tfstate"))
	writeFixtureFile(t, filepath.Join(locked, "main.tf"), lockedWorkspace)
	writeFixtureFile(t, filepath.Join(locked, "item", "main.tf"), lockedItemModule)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "ansible.cfg"), lockedAnsibleConfig)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "inventory", "hosts.yml"), lockedInventory)

	initArgs := tofuArgs("locked", "init", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, initArgs...), initArgs...)
	return tree
}

func planTarget(t *testing.T, tree configsTree, module, planFile string) {
	t.Helper()
	args := tofuArgs("locked", "plan", "-input=false", "-target="+module, "-out="+planFile)
	requireSuccess(t, runConfigsctl(t, tree, args...), args...)
}

func TestTofuApplyOfSavedPlanLocksTheDeclaredHost(t *testing.T) {
	requireTofu(t)
	tree := newLockedTree(t)
	planTarget(t, tree, "module.alpha", "alpha.tfplan")

	result := runConfigsctl(t, tree, tofuArgs("locked", "apply", "-input=false", "alpha.tfplan")...)
	if result.exitCode == 0 {
		t.Fatalf("apply succeeded, want a refusal for hv_alpha without an ssh lock target\nstderr: %s", result.stderr)
	}
	if !strings.Contains(result.stderr, "tofu.lock.plan_hosts") || !strings.Contains(result.stderr, "hosts=[hv_alpha]") {
		t.Fatalf("stderr = %q, want the plan lock set [hv_alpha]", result.stderr)
	}
	if !strings.Contains(result.stderr, "host hv_alpha from tofu.lock_targets has no ssh lock target") {
		t.Fatalf("stderr = %q, want the refusal for hv_alpha", result.stderr)
	}
	requireNoFile(t, filepath.Join(tree.workspaces, "locked", "locked.tfstate"))
}

func TestTofuApplyOfUnmatchedPlanLocksEveryHypervisor(t *testing.T) {
	requireTofu(t)
	tree := newLockedTree(t)
	planTarget(t, tree, "module.gamma", "gamma.tfplan")

	args := tofuArgs("locked", "apply", "-input=false", "gamma.tfplan")
	result := runConfigsctl(t, tree, args...)
	requireSuccess(t, result, args...)
	if !strings.Contains(result.stderr, "tofu.lock.all_hypervisors") || !strings.Contains(result.stderr, "matches no lock target") {
		t.Fatalf("stderr = %q, want the fallback to every hypervisor", result.stderr)
	}
	requireFile(t, filepath.Join(tree.workspaces, "locked", "locked.tfstate"))
}
