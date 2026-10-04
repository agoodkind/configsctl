package vault

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	ansiblevault "github.com/sosedoff/ansible-vault-go"
)

func TestRenameSecret(t *testing.T) {
	dir := t.TempDir()
	const vaultPhrase = "fixture-phrase"
	passwordFile := filepath.Join(dir, "vault.pass")
	if err := os.WriteFile(passwordFile, []byte(vaultPhrase+"\n"), 0o600); err != nil {
		t.Fatalf("write password file: %v", err)
	}
	vaultFile := filepath.Join(dir, "vault.yml")
	if err := ansiblevault.EncryptFile(vaultFile, "alpha: one\nbeta: two\n", vaultPhrase); err != nil {
		t.Fatalf("encrypt vault: %v", err)
	}

	if err := RenameSecret("alpha", "gamma", vaultFile, passwordFile); err != nil {
		t.Fatalf("RenameSecret(alpha, gamma): %v", err)
	}
	names, err := Keys(vaultFile, passwordFile)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if want := []string{"beta", "gamma"}; !slices.Equal(names, want) {
		t.Fatalf("Keys = %v, want %v", names, want)
	}
	value, err := Secret("gamma", vaultFile, passwordFile)
	if err != nil {
		t.Fatalf("Secret(gamma): %v", err)
	}
	if value != "one" {
		t.Fatalf("Secret(gamma) = %q, want %q", value, "one")
	}

	if err := RenameSecret("gamma", "beta", vaultFile, passwordFile); err == nil {
		t.Fatal("RenameSecret(gamma, beta) succeeded, want an error for an existing name")
	}
	if err := RenameSecret("missing", "delta", vaultFile, passwordFile); err == nil {
		t.Fatal("RenameSecret(missing, delta) succeeded, want an error for a missing name")
	}
}

func TestDeleteSecrets(t *testing.T) {
	dir := t.TempDir()
	const vaultPhrase = "fixture-phrase"
	passwordFile := filepath.Join(dir, "vault.pass")
	if err := os.WriteFile(passwordFile, []byte(vaultPhrase+"\n"), 0o600); err != nil {
		t.Fatalf("write password file: %v", err)
	}
	vaultFile := filepath.Join(dir, "vault.yml")
	if err := ansiblevault.EncryptFile(vaultFile, "alpha: one\nbeta: two\ngamma: three\n", vaultPhrase); err != nil {
		t.Fatalf("encrypt vault: %v", err)
	}

	if err := DeleteSecrets([]string{"alpha", "missing"}, vaultFile, passwordFile); err == nil {
		t.Fatal("DeleteSecrets(alpha, missing) succeeded, want an error for a missing name")
	}
	if err := DeleteSecrets([]string{"alpha", "gamma"}, vaultFile, passwordFile); err != nil {
		t.Fatalf("DeleteSecrets(alpha, gamma): %v", err)
	}
	names, err := Keys(vaultFile, passwordFile)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if want := []string{"beta"}; !slices.Equal(names, want) {
		t.Fatalf("Keys = %v, want %v", names, want)
	}
}
