//go:build unix

package main_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeLintSplitInline(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		source string
	}{
		{
			"main.tf",
			`resource "terraform_data" "files" {
 provisioner "remote-exec" {
  inline = [
   "echo prepare",
   "for file in a b; do",
   "install -m 0644 $file /tmp/",
   "done"
  ]
 }
}
`,
		},
		{
			"main.tf.json",
			`{"resource":{"terraform_data":{"files":{"provisioner":{"remote-exec":{
 "inline":[
  "echo prepare",
  "for file in a b; do",
  "install -m 0644 $file /tmp/",
  "done"
 ]
}}}}}}
`,
		},
	} {
		t.Run(
			fixture.name,
			func(t *testing.T) {
				tree := nativeLintTree(t)
				path := filepath.Join(tree.workspaces, "modules", "split", fixture.name)
				writeFixtureFile(t, path, fixture.source)
				formatNativeLintTree(t, tree)
				contents, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}

				line := 1 + strings.Count(strings.Split(string(contents), "for file")[0], "\n")
				result := runConfigsctl(t, tree, "lint")
				requireSuccess(t, result)
				if strings.Count(result.stderr, "(file_install_loop)") != 1 ||
					!strings.Contains(result.stderr, fmt.Sprintf("%s:%d:", fixture.name, line)) {
					t.Fatalf("lint missed the split loop header location: %s", result.stderr)
				}
			},
		)
	}
}

func TestNativeLintLocalExec(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		source string
	}{
		{
			"main.tf",
			`resource "terraform_data" "files" {
 provisioner "local-exec" {
  command = "for file in a b; do cp $file /tmp/private-install-target/; done"
 }
 provisioner "local-exec" {
  command = "for file in a b; do test -r $file; done"
 }
}
`,
		},
		{
			"heredoc.tf",
			`resource "terraform_data" "files" {
 provisioner "local-exec" {
  command = <<-SHELL
    echo prepare
    for file in a b; do
      cp $file /tmp/private-install-target/
    done
  SHELL
 }
 provisioner "local-exec" {
  command = "for file in a b; do test -r $file; done"
 }
}
`,
		},
		{
			"main.tf.json",
			`{"resource":{"terraform_data":{"files":{"provisioner":[
 {"local-exec":{"command":"for file in a b; do cp $file /tmp/private-install-target/; done"}},
 {"local-exec":{"command":"for file in a b; do test -r $file; done"}}
]}}}}`,
		},
	} {
		t.Run(
			fixture.name,
			func(t *testing.T) {
				tree := nativeLintTree(t)
				path := filepath.Join(tree.workspaces, "modules", "local", fixture.name)
				writeFixtureFile(t, path, fixture.source)
				formatNativeLintTree(t, tree)
				contents, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}

				line := 1 + strings.Count(strings.Split(string(contents), "for file")[0], "\n")
				result := runConfigsctl(t, tree, "lint")
				requireSuccess(t, result)
				if strings.Count(result.stderr, "(file_install_loop)") != 1 ||
					!strings.Contains(result.stderr, fmt.Sprintf("%s:%d:", fixture.name, line)) ||
					!strings.Contains(
						result.stderr,
						"warning: review a declared file collection",
					) ||
					strings.Contains(result.stderr, "private-install-target") {
					t.Fatalf(
						"lint did not report the local installation advisory privately: %s",
						result.stderr,
					)
				}
			},
		)
	}
}

func TestNativeLintShellLoopBodies(t *testing.T) {
	tree := nativeLintTree(t)
	const filename = "loops.tofu"
	const source = `resource "terraform_data" "files" {
  provisioner "local-exec" {
    command = <<-SHELL
      for outer in a b; do
        for inner in c d; do
          test -r "$inner"
        done
        '/usr/bin/install' "$outer" /tmp/
      done
      for quoted in a b; do
        echo "done; cp fake /tmp/"
        "cp" "$quoted" /tmp/
      done
      for mixed in a b; do
        m'v' "$mixed" /tmp/
      done
      for polling in a b; do
        echo "status; install fake /tmp/"
        # mv fake /tmp/
        test -r "$polling"
      done
      for dynamic in a b; do
        "$directory/cp" "$dynamic" /tmp/
      done
      mv outside /tmp/
    SHELL
  }
}
`
	path := filepath.Join(tree.workspaces, "modules", "loops", filename)
	writeFixtureFile(t, path, source)
	formatNativeLintTree(t, tree)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	result := runConfigsctl(t, tree, "lint")
	requireSuccess(t, result)
	if strings.Count(result.stderr, "(file_install_loop)") != 3 {
		t.Fatalf("lint reported unexpected installation advisories: %s", result.stderr)
	}

	for _, loop := range []string{"for outer", "for quoted", "for mixed"} {
		line := 1 + strings.Count(strings.Split(string(contents), loop)[0], "\n")
		expected := fmt.Sprintf("%s:%d:", filename, line)
		if !strings.Contains(result.stderr, expected) {
			t.Fatalf("lint missed installation advisory at %s: %s", expected, result.stderr)
		}
	}
}

func TestNativeLintCheckAssertionParity(t *testing.T) {
	for _, fixture := range []struct {
		filename string
		source   string
	}{
		{
			"check.tf",
			`variable "items" {
  type = map(string)
}
check "items" {
  assert {
    condition     = length([for key in sort(keys(var.items)) : key]) > 0
    error_message = "Supply an item."
  }
}
`,
		},
		{
			"check.tf.json",
			`{
  "variable": {"items": {"type": "map(string)"}},
  "check": {"items": {"assert": [{
    "condition": "${length([for key in sort(keys(var.items)) : key]) > 0}",
    "error_message": "Supply an item."
  }]}}
}
`,
		},
	} {
		t.Run(
			fixture.filename,
			func(t *testing.T) {
				tree := nativeLintTree(t)
				path := filepath.Join(tree.workspaces, "modules", "assertion", fixture.filename)
				writeFixtureFile(t, path, fixture.source)
				formatNativeLintTree(t, tree)

				line := 1 + strings.Count(strings.Split(fixture.source, "sort(keys")[0], "\n")
				expected := fmt.Sprintf("%s:%d:", fixture.filename, line)
				result := runConfigsctl(t, tree, "lint")
				if result.exitCode == 0 ||
					strings.Count(result.stderr, "(redundant_sorted_keys)") != 1 ||
					!strings.Contains(result.stderr, expected) {
					t.Fatalf("lint missed the check assertion at %s: %s", expected, result.stderr)
				}

				corrected := strings.Replace(
					fixture.source,
					"sort(keys(var.items))",
					"keys(var.items)",
					1,
				)
				writeFixtureFile(t, path, corrected)
				formatNativeLintTree(t, tree)
				requireSuccess(t, runConfigsctl(t, tree, "lint"))
			},
		)
	}
}

func TestNativeLintExpressionFieldNames(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		source string
	}{
		{
			"main.tf",
			`locals {
 description = [for key in sort(keys({a=1})) : key]
 error_message = [for key in sort(keys({b=2})) : key]
}
output "values" { value = [local.description, local.error_message] }
output "metadata" {
 description = "/etc/metadata.conf"
 value = "/etc/metadata.conf"
}
`,
		},
		{
			"main.tf.json",
			`{
 "locals": {
  "description":"${[for key in sort(keys({a=1})) : key]}",
  "error_message":"${[for key in sort(keys({b=2})) : key]}"
 },
 "output": {
  "values":{"value":"${[local.description,local.error_message]}"},
  "metadata":{"description":"/etc/metadata.conf","value":"/etc/metadata.conf"}
 }
}`,
		},
	} {
		t.Run(
			fixture.name,
			func(t *testing.T) {
				tree := nativeLintTree(t)
				path := filepath.Join(tree.workspaces, "modules", "fields", fixture.name)
				writeFixtureFile(t, path, fixture.source)
				formatNativeLintTree(t, tree)
				result := runConfigsctl(t, tree, "lint")
				if result.exitCode == 0 ||
					strings.Count(result.stderr, "(redundant_sorted_keys)") != 2 ||
					strings.Contains(result.stderr, "(repeated_source_literal)") {
					t.Fatalf(
						"lint skipped ordinary expressions or checked literal metadata: %s",
						result.stderr,
					)
				}
			},
		)
	}
}

func TestNativeLintDuplicateGroups(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		source string
	}{
		{
			"main.tf",
			`output "first" { value = { service = "mwan" } }
output "second" { value = "/etc/group.conf" }
output "third" { value = { service = "alternate" } }
output "fourth" { value = { service = "mwan" } }
output "fifth" { value = "/etc/group.conf" }
output "sixth" { value = { service = "alternate" } }
output "plain_first" { value = "mwan" }
output "plain_second" { value = "mwan" }
`,
		},
		{
			"main.tf.json",
			`{"output":{
 "first":{"value":{"service":"mwan"}},
 "second":{"value":"/etc/group.conf"},
 "third":{"value":{"service":"alternate"}},
 "fourth":{"value":{"service":"mwan"}},
 "fifth":{"value":"/etc/group.conf"},
 "sixth":{"value":{"service":"alternate"}},
 "plain_first":{"value":"mwan"},
 "plain_second":{"value":"mwan"}
}}`,
		},
	} {
		t.Run(
			fixture.name,
			func(t *testing.T) {
				tree := nativeLintTree(t)
				path := filepath.Join(tree.workspaces, "modules", "groups", fixture.name)
				writeFixtureFile(t, path, fixture.source)
				formatNativeLintTree(t, tree)
				result := runConfigsctl(t, tree, "lint")
				requireSuccess(t, result)
				if strings.Count(result.stderr, "(repeated_source_literal)") != 6 ||
					strings.Contains(result.stderr, "mwan") ||
					strings.Contains(result.stderr, "alternate") ||
					strings.Contains(result.stderr, "/etc/group.conf") {
					t.Fatalf("lint lost groups or printed literal values: %s", result.stderr)
				}

				firstLine := 1
				if strings.HasSuffix(fixture.name, ".json") {
					firstLine++
				}

				for _, group := range []struct {
					category string
					line     int
				}{
					{"service identifier", firstLine},
					{"path", firstLine + 1},
					{"service identifier", firstLine + 2},
				} {
					expected := fmt.Sprintf(
						"warning: repeated %s; first occurrence at workspaces/modules/groups/%s:%d:",
						group.category,
						fixture.name,
						group.line,
					)
					if strings.Count(result.stderr, expected) != 2 {
						t.Fatalf(
							"lint did not associate interleaved duplicate locations: %s",
							result.stderr,
						)
					}
				}
			},
		)
	}
}
