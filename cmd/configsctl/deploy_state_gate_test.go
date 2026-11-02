package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pendingImportID = "synthetic-private-import-identity-never-print"

func gateStateFixture(t *testing.T, name string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read gate fixture %s: %v", name, err)
	}
	return string(contents)
}

func TestDeployIdentifiesPendingGuestImport(t *testing.T) {
	tree := newGatedTree(t, true)
	applyArgs := tofuArgs("guest", "apply", "-auto-approve", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, applyArgs...), applyArgs...)

	guest := filepath.Join(tree.workspaces, "guest")
	writeFixtureFile(t, filepath.Join(guest, "item", "import.tf"), gateStateFixture(t, "gate-import-resource.tf"))
	writeFixtureFile(t, filepath.Join(guest, "import.tf"), gateStateFixture(t, "gate-import.tf"))

	result := runConfigsctl(t, tree, "deploy", gatedPlaybookName)
	requireRefusal(t, tree, result, pendingRefusal, `module.base["guest_alpha"].terraform_data.imported`)
	if strings.Contains(eventLines(result.stderr), pendingImportID) {
		t.Fatal("the gate diagnostic exposed the import ID")
	}
}

func TestDeployIdentifiesPendingGuestMove(t *testing.T) {
	tree := newGatedTree(t, true)
	applyArgs := tofuArgs("guest", "apply", "-auto-approve", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, applyArgs...), applyArgs...)

	guest := filepath.Join(tree.workspaces, "guest")
	writeFixtureFile(t, filepath.Join(guest, "item", "main.tf"), gateStateFixture(t, "gate-moved-item.tf"))

	result := runConfigsctl(t, tree, "deploy", gatedPlaybookName)
	requireRefusal(t, tree, result, pendingRefusal, `module.base["guest_alpha"].terraform_data.renamed`)
}
