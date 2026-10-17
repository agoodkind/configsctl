package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const absentGuestInventory = `all:
  children:
    guests:
      hosts:
        present.home.example:
          ansible_connection: local
          service_vmid: 116
        missing.home.example:
          ansible_connection: local
          service_vmid: "117"
        other.away.example:
          ansible_connection: local
          service_vmid: 999
    proxmox_guests:
      hosts:
        present:
          ansible_connection: local
          ansible_proxmox_host: vault
          ansible_proxmox_vmid: "116"
        elsewhere:
          ansible_connection: local
          ansible_proxmox_host: suburban
          ansible_proxmox_vmid: 117
`

const absentGuestPlaybook = `- name: Write the host marker
  hosts: guests
  gather_facts: false
  tasks:
    - name: Write the host marker
      ansible.builtin.copy:
        content: "played\n"
        dest: "{{ playbook_dir }}/played-{{ inventory_hostname }}"
        mode: "0600"
`

const (
	absentGuestPlaybookName = "deploy-absent"
	presentGuestHost        = "present.home.example"
	missingGuestHost        = "missing.home.example"
	unmappedGuestHost       = "other.away.example"
	enrolledGuestSettings   = "  lock_targets:\n    workspaces/guest:\n      guest_hosts:\n        vault/117: " +
		missingGuestHost + "\n"
)

func absentGuestNodes(label, node string) string {
	return "deploy:\n  absent_guest_nodes:\n    " + label + ": " + node + "\n"
}

func newAbsentGuestTree(t *testing.T, settings string) configsTree {
	t.Helper()
	requireAnsiblePlaybook(t)
	tree := newConfigsTree(t)
	writeFixtureFile(t, filepath.Join(tree.root, "configsctl.yml"), settingsContent+settings)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "ansible.cfg"), lockedAnsibleConfig)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "inventory", "hosts.yml"), absentGuestInventory)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "playbooks", absentGuestPlaybookName+".yml"),
		absentGuestPlaybook)
	return tree
}

func hostMarker(tree configsTree, host string) string {
	return filepath.Join(tree.root, "ansible", "playbooks", "played-"+host)
}

func requireStderr(t *testing.T, result commandResult, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(result.stderr, fragment) {
			t.Fatalf("stderr = %q, want it to mention %q", result.stderr, fragment)
		}
	}
}

func requireDeployRefusal(t *testing.T, tree configsTree, result commandResult, fragments ...string) {
	t.Helper()
	if result.exitCode == 0 {
		t.Fatalf("Deploy succeeded. The test expected a refusal.\nstderr: %s", result.stderr)
	}
	requireStderr(t, result, fragments...)
	if strings.Contains(result.stderr, "hostlock.") {
		t.Fatalf("stderr = %q, want no lock work before the refusal", result.stderr)
	}
	for _, host := range []string{presentGuestHost, missingGuestHost, unmappedGuestHost} {
		requireNoFile(t, hostMarker(tree, host))
	}
}

func readRunLogs(t *testing.T, tree configsTree) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(tree.root, "tmp", "configs-runs", "*.log"))
	if err != nil {
		t.Fatalf("list run logs: %v", err)
	}
	var combined strings.Builder
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read run log %s: %v", path, err)
		}
		combined.Write(body)
	}
	return combined.String()
}

func requireDeploySuccess(t *testing.T, tree configsTree, result commandResult, args ...string) {
	t.Helper()
	if result.exitCode != 0 {
		t.Fatalf("configsctl %v exited %d\nstderr: %s\nrun log: %s",
			args, result.exitCode, result.stderr, readRunLogs(t, tree))
	}
}

// Ansible on Python 3.12 binds a socket under TMPDIR, and TMPDIR contains the
// test name. Each test that runs a play keeps its name at 38 characters or
// fewer, because a longer TMPDIR exceeds the 108-byte socket path limit of Linux.
func TestDeploySkipsAHostWithNoProxmoxGuest(t *testing.T) {
	tree := newAbsentGuestTree(t, absentGuestNodes("home", "vault"))

	result := runConfigsctl(t, tree, "deploy", absentGuestPlaybookName)
	requireDeploySuccess(t, tree, result, "deploy", absentGuestPlaybookName)
	requireStderr(t, result,
		"deploy.hosts_absent hosts=["+missingGuestHost+"] nodes=[vault] vmids=[117]",
		`hostlock.targets_skipped hosts="[`+unmappedGuestHost+" "+presentGuestHost+`]"`)
	if want := "Skipping hosts with no Proxmox guest: " + missingGuestHost; !strings.Contains(readRunLogs(t, tree), want) {
		t.Fatalf("run log does not contain %q", want)
	}
	requireFile(t, hostMarker(tree, presentGuestHost))
	requireFile(t, hostMarker(tree, unmappedGuestHost))
	requireNoFile(t, hostMarker(tree, missingGuestHost))
}

func TestDeployCommaLimitSkipsAnAbsentHost(t *testing.T) {
	tree := newAbsentGuestTree(t, absentGuestNodes("home", "vault"))
	limit := missingGuestHost + "," + presentGuestHost

	result := runConfigsctl(t, tree, "deploy", absentGuestPlaybookName, "--limit", limit)
	requireDeploySuccess(t, tree, result, "deploy", absentGuestPlaybookName, "--limit", limit)
	requireStderr(t, result, "deploy.hosts_absent hosts=["+missingGuestHost+"]")
	requireFile(t, hostMarker(tree, presentGuestHost))
	requireNoFile(t, hostMarker(tree, missingGuestHost))
	requireNoFile(t, hostMarker(tree, unmappedGuestHost))
}

func TestDeployCheckModeSkipsAnAbsentHost(t *testing.T) {
	tree := newAbsentGuestTree(t, absentGuestNodes("home", "vault"))

	result := runConfigsctl(t, tree, "deploy", absentGuestPlaybookName, "--check")
	requireDeploySuccess(t, tree, result, "deploy", absentGuestPlaybookName, "--check")
	requireStderr(t, result, "deploy.hosts_absent hosts=["+missingGuestHost+"]")
	if strings.Contains(result.stderr, "hostlock.") {
		t.Fatalf("stderr = %q, want no lock work in check mode", result.stderr)
	}
}

func TestDeployRefusesANodeWithNoProxmoxGuests(t *testing.T) {
	tree := newAbsentGuestTree(t, absentGuestNodes("home", "poweredge"))

	result := runConfigsctl(t, tree, "deploy", absentGuestPlaybookName)
	requireDeployRefusal(t, tree, result, "the Proxmox inventory returned no guests for node poweredge")
}

func TestDeployRefusesAnAbsentEnrolledGuest(t *testing.T) {
	tree := newAbsentGuestTree(t, enrolledGuestSettings+absentGuestNodes("home", "vault"))

	result := runConfigsctl(t, tree, "deploy", absentGuestPlaybookName)
	requireDeployRefusal(t, tree, result,
		"host "+missingGuestHost+" is a value of tofu.guest_hosts, and node vault reports no guest with VMID 117")
}

func TestDeployRefusesWhenEveryPlayHostIsAbsent(t *testing.T) {
	tree := newAbsentGuestTree(t, absentGuestNodes("home", "vault"))

	result := runConfigsctl(t, tree, "deploy", absentGuestPlaybookName, "--limit", missingGuestHost)
	requireDeployRefusal(t, tree, result, "every play host has no Proxmox guest: "+missingGuestHost)
}

func TestDeployWithoutGuestNodesKeepsHosts(t *testing.T) {
	tree := newAbsentGuestTree(t, "")

	result := runConfigsctl(t, tree, "deploy", absentGuestPlaybookName)
	requireDeploySuccess(t, tree, result, "deploy", absentGuestPlaybookName)
	requireStderr(t, result,
		`hostlock.targets_skipped hosts="[`+missingGuestHost+" "+unmappedGuestHost+" "+presentGuestHost+`]"`)
	if strings.Contains(result.stderr, "deploy.hosts_absent") {
		t.Fatalf("stderr = %q, want no absent hosts without deploy.absent_guest_nodes", result.stderr)
	}
	for _, host := range []string{presentGuestHost, missingGuestHost, unmappedGuestHost} {
		requireFile(t, hostMarker(tree, host))
	}
}
