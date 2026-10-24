package main_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func workspaceFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "workspaces", name))
	if err != nil {
		t.Fatalf("read workspace fixture %s: %v", name, err)
	}
	return string(data)
}

func independentGatedTree(t *testing.T) configsTree {
	t.Helper()
	tree := newGatedTree(t, true)
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), workspaceFixture(t, "guest-modules.yml"))
	root := filepath.Join(tree.workspaces, "suburban", "tack")
	writeFixtureFile(t, filepath.Join(root, "backend.tf"), fmt.Sprintf(localBackend, "tack.tfstate"))
	writeFixtureFile(t, filepath.Join(root, "main.tf"), workspaceFixture(t, "gate.tf"))
	writeFixtureFile(t, filepath.Join(root, "item", "main.tf"), gatedItemModule)
	applyWorkspace(t, tree, "suburban/tack")
	return tree
}

const settingsName = "configsctl.yml"

func TestDeployChecksOnlyTheOwningWorkspace(t *testing.T) {
	tree := independentGatedTree(t)
	mwan := filepath.Join(tree.workspaces, "suburban", "mwan")
	writeFixtureFile(t, filepath.Join(mwan, "backend.tf"), fmt.Sprintf(localBackend, "mwan.tfstate"))
	writeFixtureFile(t, filepath.Join(mwan, "main.tf"), workspaceFixture(t, "unrelated-counted.tf"))
	applyWorkspace(t, tree, "suburban/mwan")
	statePath := filepath.Join(mwan, "mwan.tfstate")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read unrelated routing state before deployment: %v", err)
	}
	writeFixtureFile(t, filepath.Join(mwan, "main.tf"), workspaceFixture(t, "unrelated-single.tf"))

	result := runConfigsctl(t, tree, "deploy", gatedPlaybookName, "--limit", "guest-alpha")
	requireSuccess(t, result, "deploy", gatedPlaybookName)
	requireFile(t, playMarker(tree))
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read unrelated routing state after deployment: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("Tack deployment changed the unrelated routing state")
	}

	if err := os.Remove(playMarker(tree)); err != nil {
		t.Fatalf("remove the previous fixture play marker: %v", err)
	}
	writeFixtureFile(t, filepath.Join(tree.workspaces, "suburban", "tack", "item", "main.tf"),
		workspaceFixture(t, "item-changed.tf"))
	result = runConfigsctl(t, tree, "deploy", gatedPlaybookName, "--limit", "guest-alpha")
	requireRefusal(t, tree, result, pendingRefusal, "terraform_data.item")
}

func TestDeployRefusesMissingOrDuplicateWorkspaceOwnership(t *testing.T) {
	for _, name := range []string{"duplicate-owners.yml", "missing-owner.yml"} {
		t.Run(name, func(t *testing.T) {
			tree := newGatedTree(t, true)
			writeFixtureFile(t, filepath.Join(tree.root, settingsName), workspaceFixture(t, name))
			result := runConfigsctl(t, tree, "deploy", gatedPlaybookName, "--limit", "guest-alpha")
			requireRefusal(t, tree, result, "guest-alpha")
		})
	}
}

func TestDeployDoesNotParseAnUnrelatedInvalidWorkspace(t *testing.T) {
	tree := independentGatedTree(t)
	writeFixtureFile(t, filepath.Join(tree.workspaces, "suburban", "mwan", "main.tf"), workspaceFixture(t, "broken.tf"))
	result := runConfigsctl(t, tree, "deploy", gatedPlaybookName, "--limit", "guest-alpha")
	requireSuccess(t, result, "deploy", gatedPlaybookName)
	requireFile(t, playMarker(tree))
}

func TestDeployRefusesANonexistentModuleOrGuestInstance(t *testing.T) {
	for _, mismatch := range []string{"module", "instance"} {
		t.Run(mismatch, func(t *testing.T) {
			tree := independentGatedTree(t)
			if mismatch == "module" {
				settings := strings.Replace(workspaceFixture(t, "guest-modules.yml"), "module.base", "module.missing", 1)
				writeFixtureFile(t, filepath.Join(tree.root, settingsName), settings)
			} else {
				writeFixtureFile(t, filepath.Join(tree.root, "ansible", "inventory", "hosts.yml"), workspaceFixture(t, "unknown-service.yml"))
			}
			result := runConfigsctl(t, tree, "deploy", gatedPlaybookName, "--limit", "guest-alpha")
			requireRefusal(t, tree, result, "is absent from the plan")
		})
	}
}

func TestDeployChecksEverySelectedRootWithRepeatedGuestKeys(t *testing.T) {
	tree := independentGatedTree(t)
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), workspaceFixture(t, "two-owners.yml"))
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "inventory", "hosts.yml"), workspaceFixture(t, "two-hosts.yml"))
	root := filepath.Join(tree.workspaces, "vault", "tack")
	writeFixtureFile(t, filepath.Join(root, "backend.tf"), fmt.Sprintf(localBackend, "tack.tfstate"))
	writeFixtureFile(t, filepath.Join(root, "main.tf"), workspaceFixture(t, "gate-beta.tf"))
	writeFixtureFile(t, filepath.Join(root, "item", "main.tf"), gatedItemModule)
	applyWorkspace(t, tree, "vault/tack")
	result := runConfigsctl(t, tree, "deploy", gatedPlaybookName)
	requireSuccess(t, result, "deploy", gatedPlaybookName)
	for _, guest := range []string{"guest_alpha", "guest_beta"} {
		if !strings.Contains(result.stderr, "deploy.gate.passed guest="+guest) {
			t.Fatalf("deployment did not check %s: %s", guest, result.stderr)
		}
	}
	requireFile(t, playMarker(tree))
}
