package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	ansiblevault "github.com/sosedoff/ansible-vault-go"
)

// TestTofuSecretEnv pins the export rule: tofuSecretEnv exports a vault entry
// only when a module file declares a variable of the same name or the entry
// name starts with a prefix from the settings file. The TOTP seed and time are
// the RFC 6238 test vector for the code 287082.
func TestTofuSecretEnv(t *testing.T) {
	dir := t.TempDir()
	moduleDir := filepath.Join(dir, "module")
	if err := os.Mkdir(moduleDir, 0o700); err != nil {
		t.Fatalf("mkdir module: %v", err)
	}
	variables := `
variable "vault_alpha" {
  type      = string
  sensitive = true
}

variable "plain_setting" {
  type    = string
  default = "value"
}
`
	providers := `
variable "vault_beta" {
  type = string
}

resource "terraform_data" "unused" {}
`
	writeTestFile(t, filepath.Join(moduleDir, "variables.tf"), variables)
	writeTestFile(t, filepath.Join(moduleDir, "providers.tf"), providers)
	jsonVariables := `{"variable": {"vault_delta": {"type": "string"}, "fixture_env_BOTH": {"type": "string"}}}`
	writeTestFile(t, filepath.Join(moduleDir, "extra.tf.json"), jsonVariables)

	settingsPath := filepath.Join(dir, "configsctl.yml")
	writeTestFile(t, settingsPath, "tofu:\n  module_dir: "+moduleDir+"\n  env_key_prefix: fixture_env_\n"+
		"  totp_key_prefix: fixture_totp_\n")
	loaded, err := loadSettings(settingsPath)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}

	const vaultPhrase = "fixture-phrase"
	passwordFile := filepath.Join(dir, "vault.pass")
	writeTestFile(t, passwordFile, vaultPhrase+"\n")
	vaultFile := filepath.Join(dir, "vault.yml")
	vaultContent := "vault_alpha: one\nvault_beta: two\nvault_gamma: three\n" +
		"fixture_env_FIXTURE_NAME: four\nvault_delta: five\nfixture_env_BOTH: six\n" +
		"fixture_totp_plain_setting: GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ\n"
	if err := ansiblevault.EncryptFile(vaultFile, vaultContent, vaultPhrase); err != nil {
		t.Fatalf("encrypt vault: %v", err)
	}

	got, err := tofuSecretEnv(loaded.Tofu, vaultFile, passwordFile, time.Unix(59, 0))
	if err != nil {
		t.Fatalf("tofuSecretEnv: %v", err)
	}
	want := []string{
		"TF_VAR_fixture_env_BOTH=six",
		"TF_VAR_plain_setting=287082",
		"TF_VAR_vault_alpha=one",
		"TF_VAR_vault_beta=two",
		"TF_VAR_vault_delta=five",
		"FIXTURE_NAME=four",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("tofuSecretEnv = %v, want %v", got, want)
	}
}

// TestLoadSettingsRejectsIncompleteFile pins that a settings file with an
// unknown key or a missing value stops the command.
func TestLoadSettingsRejectsIncompleteFile(t *testing.T) {
	cases := map[string]string{
		"missing prefix": "tofu:\n  module_dir: opentofu\n",
		"missing module": "tofu:\n  env_key_prefix: fixture_env_\n",
		"unknown key":    "tofu:\n  module_dir: opentofu\n  env_key_prefix: fixture_env_\n  extra: 1\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "configsctl.yml")
			writeTestFile(t, path, content)
			if _, err := loadSettings(path); err == nil {
				t.Fatalf("loadSettings accepted %q", content)
			}
		})
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
