//go:build unix

package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFmtFormatsRecursivelyWithoutVault(t *testing.T) {
	tree := newConfigsTree(t)
	path := filepath.Join(tree.workspaces, "nested", "main.tf")
	writeFixtureFile(t, path, "variable \"example\" {\n type=string\n}\n")
	writeFixtureFile(
		t,
		filepath.Join(tree.root, "ansible/inventory/group_vars/all/vault.yml"),
		"invalid",
	)
	requireSuccess(t, runConfigsctl(t, tree, "fmt"))
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(first) != "variable \"example\" {\n  type = string\n}\n" {
		t.Fatalf("unexpected formatting: %s", first)
	}

	requireSuccess(t, runConfigsctl(t, tree, "fmt"))
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(first) != string(second) {
		t.Fatal("the second fmt changed the file")
	}

	invalid := runConfigsctl(t, tree, "fmt", "extra")
	if invalid.exitCode == 0 || !strings.Contains(invalid.stderr, "usage: configsctl fmt") {
		t.Fatalf("fmt accepted an extra argument: %s", invalid.stderr)
	}

	missing := runConfigsctlWithPath(t, tree, t.TempDir(), "fmt")
	if missing.exitCode == 0 || !strings.Contains(missing.stderr, "tofu") {
		t.Fatalf("fmt did not report missing OpenTofu: %s", missing.stderr)
	}
}

func nativeLintTree(t *testing.T) configsTree {
	t.Helper()
	tree := newConfigsTree(t)
	installNativeConfiguration(t, tree)
	root := filepath.Join(tree.workspaces, "suburban", "tack")
	writeFixtureFile(t, filepath.Join(root, "backend.tf"), readCredentialFixture(t, "backend.tf"))
	writeFixtureFile(t, filepath.Join(root, "main.tf"), workspaceFixture(t, "gate.tf"))
	writeFixtureFile(t, filepath.Join(root, "item/main.tf"), gatedItemModule)
	writeFixtureFile(
		t,
		filepath.Join(tree.workspaces, "retained/backend.tf"),
		readCredentialFixture(t, "backend.tf"),
	)
	formatNativeLintTree(t, tree)

	return tree
}

func formatNativeLintTree(t *testing.T, tree configsTree) {
	t.Helper()
	command := exec.CommandContext(
		t.Context(),
		"tofu",
		"-chdir="+tree.workspaces,
		"fmt",
		"-recursive",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("format the native lint fixture: %v\n%s", err, output)
	}
}

func TestNativeLintChecksRetainedBackendSyntax(t *testing.T) {
	tree := nativeLintTree(t)
	requireSuccess(t, runConfigsctl(t, tree, "lint"))
	mutation := runConfigsctl(t, tree, "tofu", "retained", "apply", "-auto-approve")
	if mutation.exitCode == 0 ||
		!strings.Contains(mutation.stderr, "evaluate native configuration for retained") {
		t.Fatal("the retained backend accepted mutation without native metadata")
	}

	writeFixtureFile(
		t,
		filepath.Join(tree.workspaces, "retained/backend.tf"),
		nativeFixtureFile(t, "invalid-backend.tf"),
	)
	invalid := runConfigsctl(t, tree, "lint")
	if invalid.exitCode == 0 || !strings.Contains(invalid.stderr, "retained/backend.tf") {
		t.Fatalf("native lint did not check retained backend syntax: %s", invalid.stderr)
	}
}

func TestNativeLintRequiresOperationalRootMetadata(t *testing.T) {
	for _, kind := range []string{"resource", "data", "module"} {
		t.Run(
			kind,
			func(t *testing.T) {
				tree := nativeLintTree(t)
				writeFixtureFile(
					t,
					filepath.Join(tree.workspaces, "retained/main.tf"),
					nativeFixtureFile(t, "operational-"+kind+".tf"),
				)
				formatNativeLintTree(t, tree)
				result := runConfigsctl(t, tree, "lint")
				if result.exitCode == 0 ||
					!strings.Contains(result.stderr, "evaluate native configuration for retained") {
					t.Fatalf(
						"native lint accepted an operational root without metadata: %s",
						result.stderr,
					)
				}
			},
		)
	}
}
