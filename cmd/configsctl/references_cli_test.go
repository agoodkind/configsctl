//go:build unix

package main_test

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeLintValidatesGuestReferencesAcrossWorkspaces(t *testing.T) {
	tree := nativeLintTree(t)
	settings := nativeFixtureFile(t, "settings.yml") + "  validate_guest_references: true\n"
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), settings)
	writeFixtureFile(
		t,
		filepath.Join(tree.root, "native", "references.tf"),
		nativeFixtureFile(t, "guestrefs-root.tf"),
	)
	owner := filepath.Join(tree.root, "native", "retained", "main.tf")
	source := nativeFixtureFile(t, "guestrefs-owner.tf")
	writeFixtureFile(t, owner, source)
	writeFixtureFile(
		t,
		filepath.Join(tree.workspaces, "retained", "main.tf"),
		nativeFixtureFile(t, "operational-resource.tf"),
	)
	consumer := filepath.Join(tree.root, "native", "suburban", "tack", "references.tf")
	refs := nativeFixtureFile(t, "guestrefs-consumer.tf")
	writeFixtureFile(t, consumer, refs)
	formatNativeLintTree(t, tree)
	requireSuccess(t, runConfigsctl(t, tree, "lint"))

	targets := filepath.Join(tree.root, "native", "suburban", "tack", "main.tf")
	originalTargets := nativeFixtureFile(t, "targets.tf")

	cases := []struct{ name, path, original, find, replace, want string }{
		{
			"removed declaration",
			owner,
			source,
			"{ guest_alpha = local.guest }",
			"{}",
			"references undeclared guest guest-alpha (site/101)",
		},
		{
			"missing declaration field",
			owner,
			source,
			"declared    =",
			"missing     =",
			"retained guest_references requires",
		},
		{
			"missing services field",
			consumer,
			refs,
			"services    =",
			"missing     =",
			"suburban/tack guest_references requires",
		},
		{
			"duplicate identity",
			owner,
			source,
			"{ guest_alpha = local.guest }",
			"{ guest_alpha = local.guest, other = local.guest }",
			"conflicts with",
		},
		{
			"missing keyed owner",
			consumer,
			refs,
			`guest_alpha = "guest_alpha"`,
			`guest_alpha = "retired"`,
			"native/suburban/tack/references.tf:6 keyed_guest_locks",
		},
		{"wrong node", consumer, refs, `node = "site"`, `node = "other"`, "other/101"},
		{"wrong VMID", consumer, refs, "vmid = 101", "vmid = 102", "site/102"},
		{
			"wrong guest lock",
			targets,
			originalTargets,
			"site/${local.guest.vmid}",
			"other/${local.guest.vmid}",
			"lock_targets.guest_hosts",
		},
		{
			"wrong keyed lock",
			targets,
			originalTargets,
			"(local.guest.key) = local.guest.hostname",
			`(local.guest.key) = "hv_alpha"`,
			"lock_targets.module_key_hosts",
		},
	}
	for _, tc := range cases {
		t.Run(
			tc.name,
			func(t *testing.T) {
				writeFixtureFile(t, tc.path, strings.Replace(tc.original, tc.find, tc.replace, 1))
				t.Cleanup(func() { writeFixtureFile(t, tc.path, tc.original) })
				result := runConfigsctl(t, tree, "lint")
				if tc.name == "removed declaration" &&
					!strings.Contains(
						result.stderr,
						"native/suburban/tack/references.tf:4 service_mapping",
					) {
					t.Fatalf("missing service mapping source line: %s", result.stderr)
				}

				if result.exitCode == 0 || !strings.Contains(result.stderr, tc.want) {
					t.Fatalf(
						"lint exit %d, expected %q in stderr: %s",
						result.exitCode,
						tc.want,
						result.stderr,
					)
				}
			},
		)
	}
}

func TestNativeLintRequiresOwnershipForKeyedLocks(t *testing.T) {
	tree := nativeLintTree(t)
	settings := nativeFixtureFile(t, "settings.yml") + "  validate_guest_references: true\n"
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), settings)
	writeFixtureFile(
		t,
		filepath.Join(tree.root, "native", "references.tf"),
		nativeFixtureFile(t, "guestrefs-root.tf"),
	)
	path := filepath.Join(tree.root, "native", "suburban", "tack", "main.tf")
	source := nativeFixtureFile(t, "guestrefs-keyed.tf")
	writeFixtureFile(t, path, source)
	result := runConfigsctl(t, tree, "lint")
	if result.exitCode == 0 ||
		!strings.Contains(
			result.stderr,
			"native/suburban/tack/main.tf:7 lock_targets.module_key_hosts",
		) {
		t.Fatalf(
			"lint accepted the departed guest or omitted the keyed lock source line: %s",
			result.stderr,
		)
	}

	writeFixtureFile(
		t,
		path,
		strings.Replace(source, `lock_host = "guest-alpha"`, `lock_host = "hv_alpha"`, 1),
	)
	requireSuccess(t, runConfigsctl(t, tree, "lint"))
}

func TestNativeLintValidatesStaticGuestLocks(t *testing.T) {
	tree := nativeLintTree(t)
	settings := nativeFixtureFile(t, "settings.yml") + "  validate_guest_references: true\n"
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), settings)
	writeFixtureFile(
		t,
		filepath.Join(tree.root, "native", "references.tf"),
		nativeFixtureFile(t, "guestrefs-root.tf"),
	)
	writeFixtureFile(
		t,
		filepath.Join(tree.workspaces, "retained", "main.tf"),
		nativeFixtureFile(t, "operational-resource.tf"),
	)
	writeFixtureFile(
		t,
		filepath.Join(tree.root, "native", "retained", "main.tf"),
		nativeFixtureFile(t, "guestrefs-node.tf"),
	)
	path := filepath.Join(tree.root, "native", "suburban", "tack", "main.tf")
	source := nativeFixtureFile(t, "guestrefs-static.tf")
	writeFixtureFile(t, path, source)
	formatNativeLintTree(t, tree)
	requireSuccess(t, runConfigsctl(t, tree, "lint"))

	swapped := strings.NewReplacer(
		`= "compose.example"`,
		`= "search.example"`,
		`= "search.example"`,
		`= "compose.example"`,
	).Replace(
		source[strings.Index(source, "  lock_targets ="):],
	)
	writeFixtureFile(t, path, source[:strings.Index(source, "  lock_targets =")]+swapped)
	result := runConfigsctl(t, tree, "lint")
	if result.exitCode == 0 ||
		!strings.Contains(result.stderr, "main.tf:13 lock_targets.module_hosts") {
		t.Fatalf("lint failed to identify the swapped destination: %s", result.stderr)
	}

	stale := strings.Replace(source, "declared     = local.managed_guests", "declared     = {}", 1)
	stale = strings.Replace(
		stale,
		"static_locks = local.static_guest_locks",
		"static_locks = {}",
		1,
	)
	writeFixtureFile(t, path, stale)
	result = runConfigsctl(t, tree, "lint")
	if result.exitCode == 0 ||
		!strings.Contains(result.stderr, "requires a guest reference or a declared node host") {
		t.Fatalf("lint accepted a static lock for a removed guest: %s", result.stderr)
	}
}
