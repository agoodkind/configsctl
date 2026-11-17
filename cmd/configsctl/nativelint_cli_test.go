//go:build unix

package main_test

import (
	"crypto/sha256"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNativeLintUpstreamSourceRules(t *testing.T) {
	for _, test := range []struct {
		rule      string
		invalid   string
		corrected string
	}{
		{
			"terraform_unused_declarations",
			"locals { unused = 1 }\n",
			"locals { used = 1 }\noutput \"used\" { value = local.used }\n",
		},
		{
			"terraform_typed_variables",
			"variable \"example\" {}\noutput \"example\" { value = var.example }\n",
			"variable \"example\" { type = string }\noutput \"example\" { value = var.example }\n",
		},
		{
			"terraform_naming_convention",
			"variable \"BadName\" { type = string }\noutput \"example\" { value = var.BadName }\n",
			"variable \"good_name\" { type = string }\noutput \"example\" { value = var.good_name }\n",
		},
		{
			"terraform_required_providers",
			"resource \"random_pet\" \"example\" {}\n",
			"terraform {\n required_providers {\n random = { source = \"hashicorp/random\", version = \"~> 3.0\" }\n }\n}\nresource \"random_pet\" \"example\" {}\n",
		},
	} {
		t.Run(
			test.rule,
			func(t *testing.T) {
				tree := nativeLintTree(t)
				path := filepath.Join(tree.workspaces, "modules", "regression", "main.tofu")
				writeFixtureFile(t, path, test.invalid)
				formatNativeLintTree(t, tree)
				before := sourceLintSnapshot(t, tree.workspaces)
				result := runConfigsctl(t, tree, "lint")
				if result.exitCode == 0 || !strings.Contains(result.stderr, test.rule) {
					t.Fatalf("lint did not reject %s: %s", test.rule, result.stderr)
				}

				if !reflect.DeepEqual(before, sourceLintSnapshot(t, tree.workspaces)) {
					t.Fatal("lint changed workspace files")
				}

				writeFixtureFile(t, path, test.corrected)
				formatNativeLintTree(t, tree)
				before = sourceLintSnapshot(t, tree.workspaces)
				requireSuccess(t, runConfigsctl(t, tree, "lint"))
				if !reflect.DeepEqual(before, sourceLintSnapshot(t, tree.workspaces)) {
					t.Fatal("successful lint changed workspace files")
				}
			},
		)
	}
}

func sourceLintSnapshot(t *testing.T, root string) map[string][sha256.Size]byte {
	t.Helper()
	files := make(map[string][sha256.Size]byte)
	err := filepath.WalkDir(
		root,
		func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}

			if entry.IsDir() {
				return nil
			}

			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			files[path] = sha256.Sum256(body)

			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	return files
}

func TestNativeLintComputedModuleAndSourcePrecedence(t *testing.T) {
	tree := nativeLintTree(t)
	root := filepath.Join(tree.workspaces, "modules", "computed")
	writeFixtureFile(t, filepath.Join(root, "main.tf"), "locals { unused = 1 }\n")
	const computed = `variable "workspace" {
  type = string
  const = true
}
locals {
  sources = {
    "suburban/tack-qa" = "../tack/qa"
    "vault/tack-prod" = "../tack/prod"
  }
}
module "selected" {
  source = lookup(local.sources, var.workspace, "../../${var.workspace}/configuration")
}
output "value" { value = module.selected }
`
	writeFixtureFile(t, filepath.Join(root, "main.tofu"), computed)
	writeFixtureFile(t, filepath.Join(root, "value.tf.json"), `{"variable":{"unused":{}}}`)
	writeFixtureFile(
		t,
		filepath.Join(root, "value.tofu.json"),
		`{"output":{"value":{"value":"constant"}}}`,
	)
	formatNativeLintTree(t, tree)
	requireSuccess(t, runConfigsctl(t, tree, "lint"))
	writeFixtureFile(t, filepath.Join(root, "main.tofu"), computed+"locals { unused = 1 }\n")
	formatNativeLintTree(t, tree)
	result := runConfigsctl(t, tree, "lint")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "terraform_unused_declarations") {
		t.Fatalf("lint ignored active OpenTofu source: %s", result.stderr)
	}

	if !strings.Contains(result.stderr, "modules/computed/main.tofu:") ||
		strings.Contains(result.stderr, "configsctl-source-lint-") {
		t.Fatalf("lint did not report the original source location: %s", result.stderr)
	}
}

func TestNativeLintMissingTFLint(t *testing.T) {
	tree := nativeLintTree(t)
	directory := t.TempDir()
	binary, err := exec.LookPath("tofu")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(binary, filepath.Join(directory, "tofu")); err != nil {
		t.Fatal(err)
	}

	result := runConfigsctlWithPath(t, tree, directory, "lint")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "install TFLint v0.66.0") {
		t.Fatalf("lint did not explain the missing dependency: %s", result.stderr)
	}
}

func TestNativeLintExplicitFilesKeepTemplateLinter(t *testing.T) {
	tree := newConfigsTree(t)
	path := filepath.Join(tree.root, "template.j2")
	writeFixtureFile(t, path, "plain template\n")
	requireSuccess(t, runConfigsctl(t, tree, "lint", path))
}

func TestNativeLintJSONDiagnostics(t *testing.T) {
	tree := nativeLintTree(t)
	path := filepath.Join(tree.workspaces, "modules", "json", "main.tofu.json")
	writeFixtureFile(
		t,
		path,
		`{"variable":{"BadName":{"type":"string"}},"output":{"value":{"value":"${var.BadName}"}}}`,
	)
	result := runConfigsctl(t, tree, "lint")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "modules/json/main.tofu.json:") ||
		!strings.Contains(result.stderr, "terraform_naming_convention") {
		t.Fatalf("lint did not report the original JSON source: %s", result.stderr)
	}

	writeFixtureFile(t, path, `{"variable":`)
	result = runConfigsctl(t, tree, "lint")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "modules/json/main.tofu.json:") {
		t.Fatalf("lint accepted invalid JSON source: %s", result.stderr)
	}
}

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
	base, err := os.MkdirTemp(shortTempRoot, "cfglint-")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(base) })
	tree := newConfigsTreeAt(t, base)
	installNativeConfiguration(t, tree)
	writeFixtureFile(
		t,
		filepath.Join(tree.workspaces, "shared/output.tf"),
		"output \"shared\" { value = var.vault_shared }\n",
	)
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
				if kind == "data" {
					writeFixtureFile(
						t,
						filepath.Join(tree.workspaces, "retained/output.tf"),
						"output \"fixture\" { value = data.terraform_remote_state.fixture.outputs }\n",
					)
				}

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
