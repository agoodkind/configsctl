package main_test

import (
	"fmt"
	"os"
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

func newLockedFixture(t *testing.T) explicitFixture {
	t.Helper()
	fixture := newDirectFixture(t, explicitSettings)
	installNativeConfiguration(t, fixture.tree)
	body, err := os.ReadFile(filepath.Join("testdata", "nativeconfiguration", "locked-targets.tf"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(fixture.tree.root, "native/locked/main.tf"), string(body))
	locked := filepath.Join(fixture.tree.workspaces, "locked")
	writeFixtureFile(t, filepath.Join(locked, "backend.tf"), fmt.Sprintf(localBackend, "locked.tfstate"))
	writeFixtureFile(t, filepath.Join(locked, "main.tf"), lockedWorkspace)
	writeFixtureFile(t, filepath.Join(locked, "item/main.tf"), lockedItemModule)
	requireSuccess(t, fixture.mutateWorkspace(t, "locked", "init", "-input=false"))
	return fixture
}

func planTarget(t *testing.T, tree configsTree, module, planFile string) {
	t.Helper()
	publishConfigsTree(t, tree)
	args := tofuArgs("locked", "plan", "-input=false", "-target="+module, "-out="+planFile)
	requireSuccess(t, runConfigsctl(t, tree, args...), args...)
}

func requireNoPlanCopies(t *testing.T, tree configsTree) {
	t.Helper()
	leftovers, err := filepath.Glob(filepath.Join(tree.root, "tmp", "configsctl-plan-*"))
	if err != nil {
		t.Fatalf("list plan copies in TMPDIR: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("TMPDIR has plan copies after configsctl exited: %v", leftovers)
	}
}

func TestTofuApplyOfSavedPlanLocksTheDeclaredHost(t *testing.T) {
	for _, flags := range [][]string{{"-input=false"}, {"-auto-approve"}} {
		t.Run(flags[0], func(t *testing.T) {
			fixture := newLockedFixture(t)
			planTarget(t, fixture.tree, "module.alpha", "alpha.tfplan")
			arguments := append([]string{"apply"}, flags...)
			arguments = append(arguments, "alpha.tfplan")
			result := fixture.mutateWorkspace(t, "locked", arguments...)
			requireSuccess(t, result)
			if !strings.Contains(result.stderr, "hosts=[hv_alpha]") {
				t.Fatalf("the saved plan did not select its native owner: %s", result.stderr)
			}
			requireNoFile(t, filepath.Join(fixture.lockDir, "deploy.lock"))
			requireFile(t, filepath.Join(fixture.tree.workspaces, "locked/locked.tfstate"))
		})
	}
}

func TestTofuApplyOfUnmatchedPlanRefusesUndeclaredOwnership(t *testing.T) {
	fixture := newLockedFixture(t)
	planTarget(t, fixture.tree, "module.gamma", "gamma.tfplan")
	result := fixture.mutateWorkspace(t, "locked", "apply", "-input=false", "gamma.tfplan")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "unknown lock ownership") {
		t.Fatalf("the saved plan accepted undeclared ownership: %s", result.stderr)
	}
	if strings.Contains(result.stderr, "hostlock.acquired") {
		t.Fatal("the unmatched saved plan acquired locks")
	}
	requireNoFile(t, filepath.Join(fixture.tree.workspaces, "locked/locked.tfstate"))
}

func TestTofuApplyOfMissingSavedPlanIsRefusedBeforeLocking(t *testing.T) {
	fixture := newLockedFixture(t)
	result := fixture.mutateWorkspace(t, "locked", "apply", "-auto-approve", "missing.plan")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "read the saved plan") {
		t.Fatalf("the CLI did not refuse the missing saved plan: %s", result.stderr)
	}
	if strings.Contains(result.stderr, "hostlock.acquired") {
		t.Fatal("the missing saved plan acquired locks")
	}
}

func TestTofuApplyWithVarFilePreservesTheNativePlanAndDeclaredLocks(t *testing.T) {
	fixture := newLockedFixture(t)
	writeFixtureFile(t, filepath.Join(fixture.tree.workspaces, "locked/extra.tfvars"), "# The fixture does not declare variables.\n")
	result := fixture.mutateWorkspace(t, "locked", "apply", "-auto-approve", "-input=false", "-var-file", "extra.tfvars", "-target=module.alpha")
	requireSuccess(t, result)
	if !strings.Contains(result.stderr, "hosts=[hv_alpha]") {
		t.Fatalf("the variable-file apply did not select its declared owner: %s", result.stderr)
	}
	requireFile(t, filepath.Join(fixture.tree.workspaces, "locked/locked.tfstate"))
}
