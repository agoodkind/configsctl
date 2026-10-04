package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	ansiblevault "github.com/sosedoff/ansible-vault-go"
)

// TestTofuSecretEnv pins the export rule: tofuSecretEnv exports a vault entry
// only when a module file declares a variable of the same name.
func TestTofuSecretEnv(t *testing.T) {
	dir := t.TempDir()
	moduleDir := filepath.Join(dir, "opentofu")
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

	const vaultPhrase = "fixture-phrase"
	passwordFile := filepath.Join(dir, "vault.pass")
	writeTestFile(t, passwordFile, vaultPhrase+"\n")
	vaultFile := filepath.Join(dir, "vault.yml")
	vaultContent := "vault_alpha: one\nvault_beta: two\nvault_gamma: three\n"
	if err := ansiblevault.EncryptFile(vaultFile, vaultContent, vaultPhrase); err != nil {
		t.Fatalf("encrypt vault: %v", err)
	}

	got, err := tofuSecretEnv(moduleDir, vaultFile, passwordFile)
	if err != nil {
		t.Fatalf("tofuSecretEnv: %v", err)
	}
	want := []string{"TF_VAR_vault_alpha=one", "TF_VAR_vault_beta=two"}
	if !slices.Equal(got, want) {
		t.Fatalf("tofuSecretEnv = %v, want %v", got, want)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
