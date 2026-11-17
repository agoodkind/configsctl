//go:build unix

package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeLintSourcePatterns(t *testing.T) {
	tree := nativeLintTree(t)
	module := filepath.Join(tree.workspaces, "modules", "patterns")
	path := filepath.Join(module, "main.tofu")
	writeFixtureFile(
		t,
		path,
		`locals {
  unused = 1
  files = { z = "last", a = "first" }
}
output "values" {
  value = [for key in sort(keys(local.files)) : key]
}
output "first" { value = "/etc/example.conf" }
output "second" { value = "/etc/example.conf" }
resource "terraform_data" "files" {
  provisioner "remote-exec" {
    inline = ["for file in a b; do cp $file /tmp/$file; done"]
    script = "${path.module}/install.sh"
  }
}
`,
	)
	installSourceScript(t, module, "source-install.sh", "install.sh")
	formatNativeLintTree(t, tree)
	result := runConfigsctl(t, tree, "lint")

	for _, rule := range []string{
		"terraform_unused_declarations",
		"redundant_sorted_keys",
		"repeated_source_literal",
		"file_install_loop",
	} {
		if result.exitCode == 0 || !strings.Contains(result.stderr, rule) {
			t.Fatalf("lint omitted %s: %s", rule, result.stderr)
		}
	}

	if strings.Count(result.stderr, "(repeated_source_literal)") != 2 ||
		strings.Contains(result.stderr, "/etc/example.conf") {
		t.Fatalf("lint produced unexpected literal diagnostics: %s", result.stderr)
	}

	if !strings.Contains(result.stderr, "main.tofu:") ||
		!strings.Contains(result.stderr, "install.sh:2:") {
		t.Fatalf("lint omitted the expected source locations: %s", result.stderr)
	}
}

func TestNativeLintLiteralScopeAndWarnings(t *testing.T) {
	tree := nativeLintTree(t)
	module := filepath.Join(tree.workspaces, "modules", "patterns")
	writeFixtureFile(
		t,
		filepath.Join(module, "main.tf"),
		"locals { unused = [for key in sort(keys({})) : key] }\n",
	)
	writeFixtureFile(
		t,
		filepath.Join(module, "main.tofu"),
		`locals { source = "https://example.test/api" }
output "first" { value = local.source }
output "second" { value = "https://example.test/api" }
output "singleton" { value = "key.example" }
output "host_first" { value = "host.example" }
output "host_second" { value = "host.example" }
output "service_first" { value = "example.service" }
output "service_second" { value = "example.service" }
output "keys" { value = { "key.example" = "value", "another.example" = "value" } }
resource "terraform_data" "checks" {
  provisioner "remote-exec" { script = "${path.module}/checks.sh" }
}
`,
	)
	installSourceScript(t, module, "source-checks.sh", "checks.sh")
	writeFixtureFile(
		t,
		filepath.Join(tree.workspaces, "modules", "independent", "main.tf"),
		"output \"host\" { value = \"key.example\" }\n",
	)
	formatNativeLintTree(t, tree)
	result := runConfigsctl(t, tree, "lint")
	requireSuccess(t, result)
	if strings.Count(result.stderr, "(repeated_source_literal)") != 6 ||
		strings.Contains(result.stderr, "redundant_sorted_keys") ||
		strings.Contains(result.stderr, "file_install_loop") ||
		strings.Contains(result.stderr, "https://example.test/api") {
		t.Fatalf("lint produced unexpected source warnings: %s", result.stderr)
	}
}

func TestNativeLintSortedKeysEmptyAndNonempty(t *testing.T) {
	for _, collection := range []string{"{}", `{ z = 1, a = 2 }`} {
		t.Run(
			collection,
			func(t *testing.T) {
				verifyKeysIteration(t, collection)
				tree := nativeLintTree(t)
				path := filepath.Join(tree.workspaces, "modules", "keys", "main.tf")
				writeFixtureFile(
					t,
					path,
					"output \"values\" { value = [for key in sort(keys("+collection+")) : key] }\n",
				)
				formatNativeLintTree(t, tree)
				result := runConfigsctl(t, tree, "lint")
				if result.exitCode == 0 || !strings.Contains(
					result.stderr,
					"redundant_sorted_keys",
				) {
					t.Fatalf("lint accepted sorted keys: %s", result.stderr)
				}

				contents, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}

				corrected := strings.Replace(string(contents), "sort(keys(", "keys(", 1)
				corrected = strings.Replace(corrected, ")) :", ") :", 1)
				writeFixtureFile(t, path, corrected)
				formatNativeLintTree(t, tree)
				requireSuccess(t, runConfigsctl(t, tree, "lint"))
			},
		)
	}
}

func verifyKeysIteration(t *testing.T, collection string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "tofu", "console", "-no-color")
	command.Dir = t.TempDir()
	command.Stdin = strings.NewReader(
		"[for key in sort(keys(" + collection + ")) : key] == [for key in keys(" + collection + ") : key]\n",
	)
	output, err := command.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "true" {
		t.Fatalf("OpenTofu did not confirm equivalent iteration: %v\n%s", err, output)
	}
}

func installSourceScript(t *testing.T, module, fixture, target string) {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}

	writeFixtureFile(t, filepath.Join(module, target), string(contents))
}

func TestNativeLintJSONSourcePatterns(t *testing.T) {
	tree := nativeLintTree(t)
	module := filepath.Join(tree.workspaces, "modules", "patterns")
	writeFixtureFile(t, filepath.Join(module, "main.tf.json"), `{"locals":{"unused":1}}`)
	writeFixtureFile(
		t,
		filepath.Join(module, "main.tofu.json"),
		`{
 "locals": {"source":"/etc/example.conf"},
 "output": {
  "first": {"value":"${local.source}"},
  "second": {"value":"/etc/example.conf"},
  "keys": {"value":"${[for key in sort(keys({z=1,a=2})) : key]}"}
 },
 "resource": {"terraform_data":{"files":{"provisioner":{"remote-exec":{
  "inline":["for file in a b; do cp $file /tmp/$file; done"],
  "script":"${path.module}/install.sh"
 }}}}}
}`,
	)
	installSourceScript(t, module, "source-install.sh", "install.sh")
	formatNativeLintTree(t, tree)
	result := runConfigsctl(t, tree, "lint")

	for _, rule := range []string{
		"redundant_sorted_keys",
		"repeated_source_literal",
		"file_install_loop",
	} {
		if result.exitCode == 0 || !strings.Contains(result.stderr, rule) {
			t.Fatalf("JSON lint omitted %s: %s", rule, result.stderr)
		}
	}

	if !strings.Contains(result.stderr, "main.tofu.json:") ||
		strings.Contains(result.stderr, "terraform_unused_declarations") {
		t.Fatalf("JSON source precedence failed: %s", result.stderr)
	}
}
