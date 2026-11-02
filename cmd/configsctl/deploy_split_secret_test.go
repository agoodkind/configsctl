package main_test

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ansiblevault "github.com/sosedoff/ansible-vault-go"
)

const (
	splitSecretVariable  = "CONFIGSCTL_TEST_SPLIT_SECRET"
	splitBetweenVariable = "CONFIGSCTL_TEST_SPLIT_BETWEEN"
	splitSecretPrefix    = "synthetic-split-marker-"
	splitSecretKey       = "vault_split_marker"
	splitSecretBytes     = 8
	splitStderrText      = "unrelated stderr text\n"
	splitWritePause      = 200 * time.Millisecond
)

func writeSplitSecret(secret, between string) int {
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

func TestDeployRedactsASecretSplitAcrossPlanWrites(t *testing.T) {
	cases := []struct {
		name    string
		between string
	}{
		{name: "stderr between the halves", between: splitStderrText},
		{name: "stdout only", between: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tree := newGatedTree(t, true)
			secret := registerSplitSecret(t, tree)
			testBinary, err := os.Executable()
			if err != nil {
				t.Fatalf("resolve the test binary: %v", err)
			}
			binDir := t.TempDir()
			if err := os.Symlink(testBinary, filepath.Join(binDir, "tofu")); err != nil {
				t.Fatalf("link the test binary as tofu: %v", err)
			}
			path := binDir + string(os.PathListSeparator) + os.Getenv("PATH")
			env := []string{splitSecretVariable + "=" + secret, splitBetweenVariable + "=" + tc.between}

			result := runConfigsctlWithEnv(t, tree, path, env, "deploy", gatedPlaybookName)
			requireRefusal(t, tree, result, failedRefusal)
			log := readPlanLog(t, result.stderr)
			half := len(secret) / 2
			shown := strings.NewReplacer(
				secret, "<SECRET>", secret[:half], "<SECRET_FIRST_HALF>", secret[half:], "<SECRET_SECOND_HALF>",
			).Replace(log)
			if strings.Contains(log, secret[:half]) || strings.Contains(log, secret[half:]) {
				t.Fatalf("the plan log contains part of the secret: %q", shown)
			}
			placeholder := "<redacted:" + splitSecretKey + ">\n"
			if !strings.Contains(log, placeholder) {
				t.Fatalf("the plan log does not contain %q: %q", placeholder, shown)
			}
			if !strings.Contains(log, tc.between) {
				t.Fatalf("the plan log does not contain %q: %q", tc.between, shown)
			}
		})
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
