package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExternalModuleChanges(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	repo := t.TempDir()
	root := filepath.Join(repo, "opentofu")
	retirementRun(t, repo, "git", "init", "-q")
	retirementSave(
		t,
		filepath.Join(repo, "configsctl.yml"),
		[]byte("tofu:\n  workspaces_dir: opentofu\n"),
	)
	retirementBackend(t, root)
	retirementInstall(t, root, "module.tf", "external.tf")
	retirementInstall(t, root, "old.tf", "moved.tf")
	base := retirementCommit(t, repo)
	retirementSave(t, filepath.Join(repo, "README.md"), []byte("Documentation.\n"))
	docs := retirementCommit(t, repo)
	if err := verifyRetirement(t, repo, base, docs); err != nil {
		t.Fatal(err)
	}

	retirementDrop(t, filepath.Join(root, "old.tf"))
	retirementInstall(t, root, "moved.tofu.json", "retire.tofu.json")
	moved := retirementCommit(t, repo)
	if err := verifyRetirement(t, repo, base, moved); err != nil {
		t.Fatal(err)
	}

	retirementDrop(t, filepath.Join(root, "moved.tofu.json"))
	retirementInstall(t, root, "changed.tf", "changed.tf")
	head := retirementCommit(t, repo)
	if err := verifyRetirement(t, repo, base, head); err == nil ||
		!strings.Contains(err.Error(), "static local source") {
		t.Fatalf("expected unresolved local graph, got %v", err)
	}
}
