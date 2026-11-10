package main_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestTofuRefusesIncompleteInstalledSource(t *testing.T) {
	for _, missing := range []string{"root", "workspace", "node", "environment"} {
		t.Run(
			missing,
			func(t *testing.T) {
				tree := newConfigsTree(t)
				configuration := settingsContent + "  installed_source:\n"
				values := map[string]string{
					"root":        tree.root,
					"workspace":   "node/service",
					"node":        "node",
					"environment": "testbed",
				}

				for _, name := range []string{"root", "workspace", "node", "environment"} {
					if name != missing {
						configuration += fmt.Sprintf("    %s: %q\n", name, values[name])
					}
				}

				writeFixtureFile(t, filepath.Join(tree.root, settingsName), configuration)
				result := runConfigsctl(t, tree, tofuArgs("alpha", "init", "-input=false")...)
				if result.exitCode == 0 ||
					!strings.Contains(
						result.stderr,
						"tofu.installed_source requires root, workspace, node, and environment",
					) {
					t.Fatalf(
						"the CLI did not refuse an installed source without %s: %s",
						missing,
						result.stderr,
					)
				}

				if strings.Contains(result.stderr, "command_started") ||
					strings.Contains(result.stderr, "pvesh") {
					t.Fatalf(
						"the CLI started a command before validating installed source settings: %s",
						result.stderr,
					)
				}
			},
		)
	}
}

func TestTofuRefusesInstalledSourceWithStaticRemoteEndpoints(t *testing.T) {
	tree := newConfigsTree(t)
	configuration := settingsContent + fmt.Sprintf(
		`  installed_source:
    root: %q
    workspace: node/service
    node: node
    environment: testbed
  lock_targets:
    workspaces/alpha:
      node_hosts:
        node: hv_alpha
      lock_endpoints:
        hv_alpha:
          address: host.example.invalid
`,
		tree.root,
	)
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), configuration)
	result := runConfigsctl(t, tree, tofuArgs("alpha", "init", "-input=false")...)
	if result.exitCode == 0 ||
		!strings.Contains(result.stderr, "use Git source for static remote lock endpoints") {
		t.Fatalf("the CLI did not refuse conflicting source modes: %s", result.stderr)
	}

	if strings.Contains(result.stderr, "command_started") ||
		strings.Contains(result.stderr, "pvesh") {
		t.Fatalf(
			"the CLI started a command before refusing conflicting source modes: %s",
			result.stderr,
		)
	}
}
