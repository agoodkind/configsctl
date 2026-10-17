package main_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
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
      check_mode: false
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
	shortTempRoot    = "/tmp"
	shortTempPattern = "cfgabs"
)

func absentGuestNodes(label, node string) string {
	return "deploy:\n  absent_guest_nodes:\n    " + label + ": " + node + "\n"
}

// absentGuestTree runs configsctl with a TMPDIR outside t.TempDir. Ansible
// starts an RPC server on a Unix socket under TMPDIR, and Linux limits a
// socket path to 108 bytes.
type absentGuestTree struct {
	configsTree
	tempDir string
}

func newAbsentGuestTree(t *testing.T, settings string) absentGuestTree {
	t.Helper()
	requireAnsiblePlaybook(t)
	tree := newConfigsTree(t)
	writeFixtureFile(t, filepath.Join(tree.root, "configsctl.yml"), settingsContent+settings)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "ansible.cfg"), lockedAnsibleConfig)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "inventory", "hosts.yml"), absentGuestInventory)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "playbooks", absentGuestPlaybookName+".yml"),
		absentGuestPlaybook)
	tempDir, err := os.MkdirTemp(shortTempRoot, shortTempPattern)
	if err != nil {
		t.Fatalf("create a short TMPDIR: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })
	return absentGuestTree{configsTree: tree, tempDir: tempDir}
}

func runAbsentDeploy(t *testing.T, tree absentGuestTree, args ...string) commandResult {
	t.Helper()
	command := exec.CommandContext(context.Background(), configsctlBinary, args...)
	command.Dir = tree.root
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + tree.home,
		"TMPDIR=" + tree.tempDir,
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()
	result := commandResult{stdout: stdout.String(), stderr: stderr.String(), exitCode: 0}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		result.exitCode = exitErr.ExitCode()
	default:
		t.Fatalf("run configsctl %v: %v", args, runErr)
	}
	return result
}

func hostMarker(tree absentGuestTree, host string) string {
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

func requireDeployRefusal(t *testing.T, tree absentGuestTree, result commandResult, fragments ...string) {
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

func readRunLogs(t *testing.T, tree absentGuestTree) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(tree.tempDir, "configs-runs", "*.log"))
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

func requireDeploySuccess(t *testing.T, tree absentGuestTree, result commandResult, args ...string) {
	t.Helper()
	if result.exitCode != 0 {
		t.Fatalf("configsctl %v exited %d\nstderr: %s\nrun log: %s",
			args, result.exitCode, result.stderr, readRunLogs(t, tree))
	}
}

func TestDeploySkipsAHostWithNoProxmoxGuest(t *testing.T) {
	tree := newAbsentGuestTree(t, absentGuestNodes("home", "vault"))

	result := runAbsentDeploy(t, tree, "deploy", absentGuestPlaybookName)
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

func TestDeployWithACommaLimitSkipsAHostWithNoProxmoxGuest(t *testing.T) {
	tree := newAbsentGuestTree(t, absentGuestNodes("home", "vault"))
	limit := missingGuestHost + "," + presentGuestHost

	result := runAbsentDeploy(t, tree, "deploy", absentGuestPlaybookName, "--limit", limit)
	requireDeploySuccess(t, tree, result, "deploy", absentGuestPlaybookName, "--limit", limit)
	requireStderr(t, result, "deploy.hosts_absent hosts=["+missingGuestHost+"]")
	requireFile(t, hostMarker(tree, presentGuestHost))
	requireNoFile(t, hostMarker(tree, missingGuestHost))
	requireNoFile(t, hostMarker(tree, unmappedGuestHost))
}

func TestDeployInCheckModeSkipsAHostWithNoProxmoxGuest(t *testing.T) {
	tree := newAbsentGuestTree(t, absentGuestNodes("home", "vault"))

	result := runAbsentDeploy(t, tree, "deploy", absentGuestPlaybookName, "--check")
	requireDeploySuccess(t, tree, result, "deploy", absentGuestPlaybookName, "--check")
	requireStderr(t, result, "deploy.hosts_absent hosts=["+missingGuestHost+"]")
	if strings.Contains(result.stderr, "hostlock.") {
		t.Fatalf("stderr = %q, want no lock work in check mode", result.stderr)
	}
	requireFile(t, hostMarker(tree, presentGuestHost))
	requireFile(t, hostMarker(tree, unmappedGuestHost))
	requireNoFile(t, hostMarker(tree, missingGuestHost))
}

func TestDeployRefusesANodeWithNoProxmoxGuests(t *testing.T) {
	tree := newAbsentGuestTree(t, absentGuestNodes("home", "poweredge"))

	result := runAbsentDeploy(t, tree, "deploy", absentGuestPlaybookName)
	requireDeployRefusal(t, tree, result, "the Proxmox inventory returned no guests for node poweredge")
}

func TestDeployRefusesAnAbsentEnrolledGuest(t *testing.T) {
	tree := newAbsentGuestTree(t, enrolledGuestSettings+absentGuestNodes("home", "vault"))

	result := runAbsentDeploy(t, tree, "deploy", absentGuestPlaybookName)
	requireDeployRefusal(t, tree, result,
		"host "+missingGuestHost+" is a value of tofu.guest_hosts, and node vault reports no guest with VMID 117")
}

func TestDeployRefusesWhenEveryPlayHostIsAbsent(t *testing.T) {
	tree := newAbsentGuestTree(t, absentGuestNodes("home", "vault"))

	result := runAbsentDeploy(t, tree, "deploy", absentGuestPlaybookName, "--limit", missingGuestHost)
	requireDeployRefusal(t, tree, result, "every play host has no Proxmox guest: "+missingGuestHost)
}

func TestDeployWithoutAbsentGuestNodesKeepsEveryPlayHost(t *testing.T) {
	tree := newAbsentGuestTree(t, "")

	result := runAbsentDeploy(t, tree, "deploy", absentGuestPlaybookName)
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
