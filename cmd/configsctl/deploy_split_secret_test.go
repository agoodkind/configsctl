package main_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	ansiblevault "github.com/sosedoff/ansible-vault-go"
)

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

func TestDeployRedactsASecretSplitAcrossPlanWrites(t *testing.T) {
	for _, tc := range splitCases {
		t.Run(tc.name, func(t *testing.T) {
			tree := newGatedTree(t, true)
			secret := registerSplitSecret(t, tree)
			path := linkTestBinary(t, "tofu")
			env := []string{splitSecretVariable + "=" + secret, splitBetweenVariable + "=" + tc.between}

			result := runConfigsctlWithEnv(t, tree, path, env, "deploy", gatedPlaybookName)
			requireRefusal(t, tree, result, failedRefusal)
			requireSplitSecretRedacted(t, readPlanLog(t, result.stderr), secret, tc.between)
		})
	}
}

func TestDeployRedactsASecretSplitAcrossPlayWrites(t *testing.T) {
	for _, tc := range splitCases {
		t.Run(tc.name, func(t *testing.T) {
			tree := newGatedTree(t, true)
			applyArgs := tofuArgs("guest", "apply", "-auto-approve", "-input=false")
			requireSuccess(t, runConfigsctl(t, tree, applyArgs...), applyArgs...)
			secret := registerSplitSecret(t, tree)
			path := linkTestBinary(t, "ansible-playbook")
			env := []string{
				splitSecretVariable + "=" + secret, splitBetweenVariable + "=" + tc.between,
				splitPlayHostVar + "=guest-alpha",
			}

			result := runConfigsctlWithEnv(t, tree, path, env, "deploy", gatedPlaybookName, "--limit", "guest-alpha")
			if !strings.Contains(result.stderr, "deploy.gate.passed guest=guest_alpha") {
				t.Fatalf("the plan gate did not pass guest_alpha\nevents:\n%s", eventLines(result.stderr))
			}
			requireSplitSecretRedacted(t, readRunLog(t, result.stdout, playLogNotice), secret, tc.between)
		})
	}
}

func TestTofuRedactsASecretSplitAcrossWrites(t *testing.T) {
	for _, tc := range splitCases {
		t.Run(tc.name, func(t *testing.T) {
			tree := newConfigsTree(t)
			secret := registerSplitSecret(t, tree)
			path := linkTestBinary(t, "tofu")
			env := []string{splitSecretVariable + "=" + secret, splitBetweenVariable + "=" + tc.between}

			result := runConfigsctlWithEnv(t, tree, path, env, "tofu", "alpha", "plan")
			requireSplitSecretRedacted(t, readRunLog(t, result.stdout, tofuLogNotice), secret, tc.between)
		})
	}
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

func TestCITofuRedactsASecretSplitAcrossWrites(t *testing.T) {
	for _, tc := range splitCases {
		t.Run(tc.name, func(t *testing.T) {
			checkout := newCICheckout(t)
			random := make([]byte, splitSecretBytes)
			if _, err := rand.Read(random); err != nil {
				t.Fatalf("generate the secret: %v", err)
			}
			secret := splitSecretPrefix + hex.EncodeToString(random)
			required := `[]`
			if tc.between != "" {
				required = `["` + splitBetweenVariable + `"]`
			}
			config := checkout.writeConfig(t, "config.json", `{"version": 1, "required_environment": `+required+
				`, "secret_environment": ["`+splitSecretVariable+`"], "secret_files": {}, "output_mode": "redacted_progress"}`)
			path := linkTestBinary(t, "tofu")
			command := exec.CommandContext(t.Context(), configsctlBinary,
				"ci", "tofu", "--config", config, "--root", ciRootName, "--workspace", ciWorkspace, "--", "validate", "-no-color")
			command.Dir = checkout.dir
			command.Env = []string{
				"PATH=" + path, "HOME=" + checkout.home, "TMPDIR=" + checkout.tmpdir, "TF_IN_AUTOMATION=1",
				splitSecretVariable + "=" + secret, splitBetweenVariable + "=" + tc.between,
			}
			var stdout, stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			if err := command.Run(); err == nil {
				t.Fatalf("ci tofu validate succeeded, want the helper exit status 1\nstderr: %s", stderr.String())
			}
			requireSplitSecretRedactedAs(t, readRunLog(t, stdout.String(), tofuLogNotice), secret, tc.between, splitSecretVariable)
			requireSplitSecretRedactedAs(t, stdout.String(), secret, "", splitSecretVariable)
		})
	}
}

func requireSplitSecretRedacted(t *testing.T, log, secret, between string) {
	t.Helper()
	requireSplitSecretRedactedAs(t, log, secret, between, splitSecretKey)
}

func requireSplitSecretRedactedAs(t *testing.T, log, secret, between, label string) {
	t.Helper()
	half := len(secret) / 2
	shown := strings.NewReplacer(
		secret, "<SECRET>", secret[:half], "<SECRET_FIRST_HALF>", secret[half:], "<SECRET_SECOND_HALF>",
	).Replace(log)
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
	if err := ansiblevault.EncryptFile(vaultFile, content+splitSecretKey+": "+secret+"\n", vaultUnlockText); err != nil {
		t.Fatalf("encrypt the vault: %v", err)
	}
	return secret
}
