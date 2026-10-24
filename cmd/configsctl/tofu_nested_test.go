package main_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nestedWorkspace(t *testing.T, tree configsTree, selector string) string {
	t.Helper()
	root := filepath.Join(tree.workspaces, filepath.FromSlash(selector))
	content, err := os.ReadFile(filepath.Join("testdata", "workspaces", "nested.tf"))
	if err != nil {
		t.Fatalf("read nested workspace fixture: %v", err)
	}
	writeFixtureFile(t, filepath.Join(root, "main.tf"), string(content))
	writeFixtureFile(t, filepath.Join(root, "backend.tf"), fmt.Sprintf(localBackend, "nested.tfstate"))
	return root
}

func TestTofuSelectsNestedWorkspaceWithoutReadingOtherRoots(t *testing.T) {
	requireTofu(t)
	tree := newConfigsTree(t)
	selected := nestedWorkspace(t, tree, "suburban/tack")
	other := nestedWorkspace(t, tree, "vault/tack")
	broken, err := os.ReadFile(filepath.Join("testdata", "workspaces", "broken.tf"))
	if err != nil {
		t.Fatalf("read unrelated invalid workspace fixture: %v", err)
	}
	writeFixtureFile(t, filepath.Join(tree.workspaces, "suburban", "mwan", "main.tf"), string(broken))

	applyWorkspace(t, tree, "suburban/tack")
	requireFile(t, filepath.Join(selected, "nested.tfstate"))
	requireNoFile(t, filepath.Join(other, "nested.tfstate"))
	resolved, err := filepath.EvalSymlinks(selected)
	if err != nil {
		t.Fatalf("resolve selected fixture root: %v", err)
	}
	if got := readOutput(t, tree, "suburban/tack", "workspace"); got != resolved {
		t.Fatalf("workspace output = %q; expected selected root %q", got, resolved)
	}
	requireOutputLength(t, tree, "suburban/tack", "alpha_length", vaultAlphaValue)
}

func TestTofuListsNestedSelectorsAndSkipsGeneratedDirectories(t *testing.T) {
	tree := newConfigsTree(t)
	nestedWorkspace(t, tree, "suburban/tack")
	nestedWorkspace(t, tree, "vault/tack")
	for _, selector := range []string{".terraform/cached", "terraform.d/provider", "modules/shared"} {
		nestedWorkspace(t, tree, selector)
	}
	if err := os.Remove(filepath.Join(tree.workspaces, "backend.tf")); err != nil {
		t.Fatalf("remove fixture root backend: %v", err)
	}
	result := runConfigsctl(t, tree, "tofu", "plan")
	if result.exitCode == 0 {
		t.Fatal("a root without a backend accepted plan")
	}
	for _, selector := range []string{"suburban/tack", "vault/tack"} {
		if !strings.Contains(result.stderr, selector) {
			t.Fatalf("workspace selection error omitted %s: %s", selector, result.stderr)
		}
	}
	for _, selector := range []string{".terraform/cached", "terraform.d/provider", "modules/shared"} {
		if strings.Contains(result.stderr, selector) {
			t.Fatalf("workspace selection error offered excluded root %s: %s", selector, result.stderr)
		}
	}
}

func TestTofuRejectsUnsafeWorkspaceSelectors(t *testing.T) {
	tree := newConfigsTree(t)
	nestedWorkspace(t, tree, "suburban/tack")
	for _, selector := range []string{"../alpha", "suburban/../../alpha", "suburban//tack", "/tmp", "modules/shared"} {
		t.Run(selector, func(t *testing.T) {
			result := runConfigsctl(t, tree, "tofu", selector, "version")
			if result.exitCode == 0 || !strings.Contains(result.stderr, "workspace selector") {
				t.Fatalf("unsafe selector %q returned %d: %s", selector, result.exitCode, result.stderr)
			}
		})
	}
}

func TestTofuRejectsWorkspaceDirectorySymlink(t *testing.T) {
	tree := newConfigsTree(t)
	nestedWorkspace(t, tree, "vault/tack")
	if err := os.Symlink(filepath.Join(tree.workspaces, "vault"), filepath.Join(tree.workspaces, "linked")); err != nil {
		t.Fatalf("create workspace escape fixture: %v", err)
	}
	result := runConfigsctl(t, tree, "tofu", "linked/tack", "version")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "without symbolic links") {
		t.Fatalf("symlink selector returned %d: %s", result.exitCode, result.stderr)
	}
}

func TestTofuRootCommandsIgnoreBackendlessCommandDirectories(t *testing.T) {
	requireTofu(t)
	tree := newConfigsTree(t)
	for _, name := range []string{"state", "output"} {
		writeFixtureFile(t, filepath.Join(tree.workspaces, name, "main.tf"), workspaceFixture(t, "nested.tf"))
	}
	applyWorkspace(t, tree, "")
	result := runConfigsctl(t, tree, "tofu", "state", "list")
	requireSuccess(t, result, "tofu", "state", "list")
	requireOutputLength(t, tree, "", "root_length", vaultRootValue)
}
