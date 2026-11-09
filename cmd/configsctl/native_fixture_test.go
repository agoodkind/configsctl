package main_test

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	ansiblevault "github.com/sosedoff/ansible-vault-go"
)

const shortTempRoot = "/tmp"

const gatedWorkspace = `
module "base" {
  source   = "./item"
  for_each = toset(["guest_alpha"])
  name     = each.key
  healthy  = %t
}
`

const gatedItemModule = `
variable "name" {
  type = string
}

variable "healthy" {
  type = bool
}

resource "terraform_data" "item" {
  input = var.name

  lifecycle {
    precondition {
      condition     = var.healthy
      error_message = "fixture precondition failed for ${var.name}"
    }
  }
}
`

const gatedInventory = `all:
  hosts:
    guest-alpha:
      ansible_connection: local
      service_name: guest_alpha
    plain-host:
      ansible_connection: local
`

const gatedSettings = `  guest_module: module.base
  lock_targets:
    workspaces/guest:
      guest_hosts:
        site/101: guest-alpha
`

const gatedPlaybook = `- name: Write the play marker
  hosts: all
  gather_facts: false
  tasks:
    - name: Write the play marker
      ansible.builtin.copy:
        content: "played\n"
        dest: "{{ playbook_dir }}/played"
        mode: "0600"
`

const (
	gatedPlaybookName   = "deploy-fixture"
	pendingRefusal      = "guest guest_alpha (host guest-alpha) has 1 pending resource changes"
	failedRefusal       = "the plan of guest guest_alpha (host guest-alpha) failed"
	planLogNotice       = "The plan output is in "
	planStreamLines     = 400000
	planStreamsVariable = "CONFIGSCTL_TEST_PLAN_STREAMS"
	stdoutMarker        = "p"
	stderrMarker        = "b"
	slogErrorLevel      = " ERROR "
	eventLineLimit      = 300
	raceReportStart     = "WARNING: DATA RACE"
	raceReportEnd       = "=================="
	panicMessage        = "panic"
	planFailedEvent     = "deploy.gate.plan_failed"
)

var (
	planStdoutLine = vaultAlphaValue[:len(vaultAlphaValue)-1]
	planStderrLine = vaultBetaValue[:len(vaultBetaValue)-1]
)

func requireAnsiblePlaybook(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Skip("ansible-playbook is not installed")
	}
}

func newGatedTree(t *testing.T, healthy bool) configsTree {
	t.Helper()
	requireTofu(t)
	requireAnsiblePlaybook(t)
	tree := newConfigsTree(t)
	writeFixtureFile(t, filepath.Join(tree.root, "configsctl.yml"), settingsContent+gatedSettings)
	guest := filepath.Join(tree.workspaces, "guest")
	writeFixtureFile(
		t,
		filepath.Join(guest, "backend.tf"),
		fmt.Sprintf(localBackend, "guest.tfstate"),
	)
	writeFixtureFile(t, filepath.Join(guest, "main.tf"), fmt.Sprintf(gatedWorkspace, healthy))
	writeFixtureFile(t, filepath.Join(guest, "item", "main.tf"), gatedItemModule)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "ansible.cfg"), lockedAnsibleConfig)
	writeFixtureFile(
		t,
		filepath.Join(tree.root, "ansible", "inventory", "hosts.yml"),
		gatedInventory,
	)
	writeFixtureFile(
		t,
		filepath.Join(tree.root, "ansible", "playbooks", gatedPlaybookName+".yml"),
		gatedPlaybook,
	)

	initArgs := tofuArgs("guest", "init", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, initArgs...), initArgs...)
	publishConfigsTree(t, tree)

	return tree
}

func playMarker(tree configsTree) string {
	return filepath.Join(tree.root, "ansible", "playbooks", "played")
}

func requireRefusal(t *testing.T, tree configsTree, result commandResult, fragments ...string) {
	t.Helper()
	if result.exitCode == 0 {
		t.Fatalf(
			"Deploy succeeded. The test expected the plan gate to refuse it.\nstderr: %s",
			result.stderr,
		)
	}

	for _, fragment := range fragments {
		if !strings.Contains(result.stderr, fragment) {
			t.Fatalf("stderr = %q, want it to mention %q", result.stderr, fragment)
		}
	}

	requireNoFile(t, playMarker(tree))
}

func writePlanStreams() int {
	var writers sync.WaitGroup

	streams := map[*os.File]string{os.Stdout: planStdoutLine, os.Stderr: planStderrLine}
	for stream, line := range streams {
		writers.Go(func() {
			_, _ = stream.WriteString(strings.Repeat(line+"\n", planStreamLines))
		})
	}

	writers.Wait()

	return 1
}

func planFailureEvent(t *testing.T, stderr string) string {
	t.Helper()

	for line := range strings.SplitSeq(stderr, "\n") {
		if strings.Contains(line, planFailedEvent) {
			return line
		}
	}

	t.Fatalf(
		"stderr does not contain the %s event\nevents:\n%s",
		planFailedEvent,
		eventLines(stderr),
	)

	return ""
}

func readPlanLog(t *testing.T, stderr string) string {
	t.Helper()
	return readRunLog(t, stderr, planLogNotice)
}

func readRunLog(t *testing.T, output, notice string) string {
	t.Helper()

	for line := range strings.SplitSeq(output, "\n") {
		path, found := strings.CutPrefix(line, notice)
		if !found {
			continue
		}

		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read the run log: %v", err)
		}

		return string(contents)
	}

	t.Fatalf("the output does not contain %q\nevents:\n%s", notice, eventLines(output))

	return ""
}

func eventLines(text string) string {
	var lines []string
	inRaceReport := false

	for line := range strings.SplitSeq(text, "\n") {
		if line == raceReportStart {
			inRaceReport = true
		}

		if inRaceReport || strings.Contains(line, slogErrorLevel) {
			lines = append(lines, line[:min(len(line), eventLineLimit)])
		}

		if line == raceReportEnd {
			inRaceReport = false
		}
	}

	return strings.Join(lines, "\n")
}

const (
	splitSecretVariable  = "CONFIGSCTL_TEST_SPLIT_SECRET"
	splitBetweenVariable = "CONFIGSCTL_TEST_SPLIT_BETWEEN"
	splitPlayHostVar     = "CONFIGSCTL_TEST_SPLIT_PLAY_HOST"
	splitSecretPrefix    = "synthetic-split-marker-"
	splitSecretKey       = "vault_split_marker"
	splitSecretBytes     = 8
	splitStderrText      = "unrelated stderr text\n"
	splitWritePause      = 200 * time.Millisecond
	listHostsFlag        = "--list-hosts"
	playLogNotice        = "Play output streams to "
	tofuLogNotice        = "Tofu output streams to "
	listHostsFormat      = "\nplaybook: fixture\n\n  play #1 (all): Fixture\tTAGS: []\n    pattern: [all]\n    hosts (1):\n      %s\n"
)

func writeSplitSecret(secret, between string, args []string) int {
	if slices.Contains(args, listHostsFlag) {
		fmt.Printf(listHostsFormat, os.Getenv(splitPlayHostVar))
		return 0
	}

	half := len(secret) / 2
	_, _ = os.Stdout.WriteString(secret[:half])
	time.Sleep(splitWritePause)
	if between != "" {
		_, _ = os.Stderr.WriteString(between)
		time.Sleep(splitWritePause)
	}

	_, _ = os.Stdout.WriteString(secret[half:] + "\n")

	return 1
}

var splitCases = []struct {
	name    string
	between string
}{
	{name: "stderr between the halves", between: splitStderrText},
	{name: "stdout only", between: ""},
}

func linkTestBinary(t *testing.T, name string) string {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve the test binary: %v", err)
	}

	binDir := t.TempDir()
	if err := os.Symlink(testBinary, filepath.Join(binDir, name)); err != nil {
		t.Fatalf("link the test binary as %s: %v", name, err)
	}

	return binDir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func requireSplitSecretRedacted(t *testing.T, log, secret, between string) {
	t.Helper()
	requireSplitSecretRedactedAs(t, log, secret, between, splitSecretKey)
}

func requireSplitSecretRedactedAs(t *testing.T, log, secret, between, label string) {
	t.Helper()
	half := len(secret) / 2
	shown := strings.NewReplacer(
		secret,
		"<SECRET>",
		secret[:half],
		"<SECRET_FIRST_HALF>",
		secret[half:],
		"<SECRET_SECOND_HALF>",
	).Replace(
		log,
	)
	if strings.Contains(log, secret[:half]) || strings.Contains(log, secret[half:]) {
		t.Fatalf("the run log contains part of the secret: %q", shown)
	}

	placeholder := "<redacted:" + label + ">\n"
	if !strings.Contains(log, placeholder) {
		t.Fatalf("the run log does not contain %q: %q", placeholder, shown)
	}

	if !strings.Contains(log, between) {
		t.Fatalf("the run log does not contain %q: %q", between, shown)
	}
}

func registerSplitSecret(t *testing.T, tree configsTree) string {
	t.Helper()
	random := make([]byte, splitSecretBytes)
	if _, err := rand.Read(random); err != nil {
		t.Fatalf("generate the secret: %v", err)
	}

	secret := splitSecretPrefix + hex.EncodeToString(random)
	vaultFile := filepath.Join(tree.root, "ansible", "inventory", "group_vars", "all", "vault.yml")
	content, err := ansiblevault.DecryptFile(vaultFile, vaultUnlockText)
	if err != nil {
		t.Fatalf("decrypt the vault: %v", err)
	}

	if err := ansiblevault.EncryptFile(
		vaultFile,
		content+splitSecretKey+": "+secret+"\n",
		vaultUnlockText,
	); err != nil {
		t.Fatalf("encrypt the vault: %v", err)
	}

	return secret
}

func workspaceFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "workspaces", name))
	if err != nil {
		t.Fatalf("read workspace fixture %s: %v", name, err)
	}

	return string(data)
}

func independentGatedTree(t *testing.T) configsTree {
	t.Helper()
	tree := newGatedTree(t, true)
	writeFixtureFile(
		t,
		filepath.Join(tree.root, settingsName),
		workspaceFixture(t, "guest-modules.yml"),
	)
	root := filepath.Join(tree.workspaces, "suburban", "tack")
	writeFixtureFile(
		t,
		filepath.Join(root, "backend.tf"),
		fmt.Sprintf(localBackend, "tack.tfstate"),
	)
	writeFixtureFile(t, filepath.Join(root, "main.tf"), workspaceFixture(t, "gate.tf"))
	writeFixtureFile(t, filepath.Join(root, "item", "main.tf"), gatedItemModule)
	applyWorkspace(t, tree, "suburban/tack")

	return tree
}

const settingsName = "configsctl.yml"
