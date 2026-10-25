package main_test

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ansiblevault "github.com/sosedoff/ansible-vault-go"
	"gopkg.in/yaml.v3"
)

func TestExportSecretsCreatesIndependentSelectedVault(t *testing.T) {
	tree := newConfigsTree(t)
	source := captureExportSource(t, tree)
	var previousUnlock string
	for _, name := range []string{"first-job", "second-job"} {
		directory := filepath.Join(t.TempDir(), name)
		result := runConfigsctl(t, tree, "export-secrets", "--directory", directory,
			"vault_alpha=job_user", "vault_alpha=job_password", "vault_beta")
		requireSuccess(t, result)
		password := readExportPassword(t, directory)
		if password == vaultUnlockText || password == previousUnlock {
			t.Fatal("export did not generate an independent unlock password")
		}
		previousUnlock = password
		vaultPath := filepath.Join(directory, "vault.yml")
		ciphertext, err := os.ReadFile(vaultPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(ciphertext, []byte("$ANSIBLE_VAULT;1.1;AES256\n")) {
			t.Fatal("export did not create an encrypted Ansible vault")
		}
		plaintext, err := ansiblevault.DecryptFile(vaultPath, password)
		if err != nil {
			t.Fatal("generated password cannot decrypt the exported vault")
		}
		var values map[string]string
		if err := yaml.Unmarshal([]byte(plaintext), &values); err != nil {
			t.Fatal("exported vault does not contain a YAML mapping")
		}
		expected := map[string]string{
			"job_user":     vaultAlphaValue,
			"job_password": vaultAlphaValue,
			"vault_beta":   vaultBetaValue,
		}
		if !maps.Equal(values, expected) {
			t.Fatal("exported vault does not contain exactly the selected and remapped values")
		}
		if _, err := ansiblevault.DecryptFile(vaultPath, vaultUnlockText); err == nil {
			t.Fatal("source unlock password decrypts the exported vault")
		}
		requireExportMode(t, directory, 0o700)
		requireExportMode(t, vaultPath, 0o600)
		requireExportMode(t, filepath.Join(directory, "vault.pass"), 0o600)
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Fatal("export directory contains unexpected files")
		}
		expectedOutput := "vault: " + vaultPath + "\npassword file: " +
			filepath.Join(directory, "vault.pass") + "\nexported 3 keys: job_password, job_user, vault_beta\n"
		if result.stdout != expectedOutput || result.stderr != "" {
			t.Fatalf("export did not report only the paths and destination keys: %s%s", result.stdout, result.stderr)
		}
		requireExportOutputPrivate(t, result, password)
		source.requireUnchanged(t)
	}
}

func TestExportSecretsRejectsInvalidSelectionsBeforeCreatingDirectory(t *testing.T) {
	tree := newConfigsTree(t)
	source := captureExportSource(t, tree)
	tests := []struct {
		name       string
		selections []string
		errorText  string
	}{
		{name: "missing key", selections: []string{"vault_alpha", "vault_missing"}, errorText: "vault key not found: vault_missing"},
		{name: "duplicate remapped destination", selections: []string{"vault_alpha=job_password", "vault_beta=job_password"}, errorText: "duplicate destination key: job_password"},
		{name: "duplicate original destination", selections: []string{"vault_alpha", "vault_beta=vault_alpha"}, errorText: "duplicate destination key: vault_alpha"},
		{name: "repeated key", selections: []string{"vault_alpha", "vault_alpha"}, errorText: "duplicate destination key: vault_alpha"},
		{name: "invalid source", selections: []string{"vault-alpha"}, errorText: "invalid key selection"},
		{name: "invalid destination", selections: []string{"vault_alpha=job-password"}, errorText: "invalid key selection"},
		{name: "empty source", selections: []string{"=job_password"}, errorText: "invalid key selection"},
		{name: "empty destination", selections: []string{"vault_alpha="}, errorText: "invalid key selection"},
		{name: "numeric destination", selections: []string{"vault_alpha=1password"}, errorText: "invalid key selection"},
		{name: "multiple separators", selections: []string{"vault_alpha=job=password"}, errorText: "invalid key selection"},
		{name: "missing selection", errorText: "usage: configsctl export-secrets"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "job")
			args := append([]string{"export-secrets", "--directory", directory}, test.selections...)
			result := runConfigsctl(t, tree, args...)
			if result.exitCode == 0 || !strings.Contains(result.stderr, test.errorText) {
				t.Fatalf("export did not reject the invalid selection: %s%s", result.stdout, result.stderr)
			}
			requireNoFile(t, directory)
			requireExportOutputPrivate(t, result)
			source.requireUnchanged(t)
		})
	}
}

func TestExportSecretsPreservesExistingDestination(t *testing.T) {
	tree := newConfigsTree(t)
	source := captureExportSource(t, tree)
	directory := filepath.Join(t.TempDir(), "existing-job")
	passwordPath := filepath.Join(directory, "vault.pass")
	vaultPath := filepath.Join(directory, "vault.yml")
	writeFixtureFile(t, passwordPath, "existing-job-unlock-password")
	writeFixtureFile(t, vaultPath, "existing-job-vault-content")
	writeFixtureFile(t, filepath.Join(directory, "unrelated"), "existing-unrelated-content")
	if err := os.Chmod(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	passwordBefore := captureExportFile(t, passwordPath)
	vaultBefore := captureExportFile(t, vaultPath)
	result := runConfigsctl(t, tree, "export-secrets", "--directory", directory, "vault_alpha")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "create new export directory") {
		t.Fatalf("export did not reject the existing directory: %s%s", result.stdout, result.stderr)
	}
	passwordBefore.requireUnchanged(t)
	vaultBefore.requireUnchanged(t)
	contents, err := os.ReadFile(filepath.Join(directory, "unrelated"))
	if err != nil || string(contents) != "existing-unrelated-content" {
		t.Fatal("export changed an unrelated file in the existing directory")
	}
	requireExportMode(t, directory, 0o750)
	source.requireUnchanged(t)
	requireExportOutputPrivate(t, result)
}

func TestExportSecretsRejectsExistingSymlinkAndFile(t *testing.T) {
	tree := newConfigsTree(t)
	for _, kind := range []string{"symlink", "file"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			destination := filepath.Join(base, "destination")
			preserved := filepath.Join(base, "original")
			writeFixtureFile(t, preserved, "preserve-existing-content")
			if kind == "symlink" {
				if err := os.Symlink(base, destination); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFixtureFile(t, destination, "preserve-existing-file")
				preserved = destination
			}
			before := captureExportFile(t, preserved)
			result := runConfigsctl(t, tree, "export-secrets", "--directory", destination, "vault_alpha")
			if result.exitCode == 0 || !strings.Contains(result.stderr, "create new export directory") {
				t.Fatalf("export did not reject the existing destination: %s%s", result.stdout, result.stderr)
			}
			before.requireUnchanged(t)
			requireNoFile(t, filepath.Join(base, "vault.yml"))
			requireNoFile(t, filepath.Join(base, "vault.pass"))
		})
	}
}

func TestExportSecretsRequiresSourceUnlockPassword(t *testing.T) {
	tree := newConfigsTree(t)
	writeFixtureFile(t, filepath.Join(tree.home, ".config", "ansible", "vault.pass"), "incorrect-fixture-unlock-password")
	source := captureExportSource(t, tree)
	directory := filepath.Join(t.TempDir(), "job")
	result := runConfigsctl(t, tree, "export-secrets", "--directory", directory, "vault_alpha")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "decrypt") {
		t.Fatalf("export did not reject the incorrect source authentication: %s%s", result.stdout, result.stderr)
	}
	requireNoFile(t, directory)
	source.requireUnchanged(t)
	requireExportOutputPrivate(t, result)
}

type exportFileSnapshot struct {
	path    string
	content []byte
	mode    os.FileMode
}

func captureExportFile(t *testing.T, path string) exportFileSnapshot {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return exportFileSnapshot{path: path, content: content, mode: info.Mode()}
}

func (snapshot exportFileSnapshot) requireUnchanged(t *testing.T) {
	t.Helper()
	current := captureExportFile(t, snapshot.path)
	if !bytes.Equal(current.content, snapshot.content) || current.mode != snapshot.mode {
		t.Fatalf("export changed the existing file %s", snapshot.path)
	}
}

type exportSourceSnapshot []exportFileSnapshot

func captureExportSource(t *testing.T, tree configsTree) exportSourceSnapshot {
	t.Helper()
	return exportSourceSnapshot{
		captureExportFile(t, filepath.Join(tree.root, "ansible", "inventory", "group_vars", "all", "vault.yml")),
		captureExportFile(t, filepath.Join(tree.home, ".config", "ansible", "vault.pass")),
	}
}

func (source exportSourceSnapshot) requireUnchanged(t *testing.T) {
	t.Helper()
	for _, snapshot := range source {
		snapshot.requireUnchanged(t)
	}
}

func readExportPassword(t *testing.T, directory string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(directory, "vault.pass"))
	if err != nil {
		t.Fatal(err)
	}
	password := strings.TrimSpace(string(content))
	if len(password) < 32 {
		t.Fatal("export unlock password is too short")
	}
	return password
}

func requireExportMode(t *testing.T, path string, expected os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != expected {
		t.Fatalf("%s permissions = %04o; expected %04o", path, info.Mode().Perm(), expected)
	}
}

func requireExportOutputPrivate(t *testing.T, result commandResult, additional ...string) {
	t.Helper()
	secrets := append([]string{vaultUnlockText, vaultAlphaValue, vaultBetaValue, vaultRootValue, vaultPrefixedValue, vaultOtherValue}, additional...)
	for _, secret := range secrets {
		if strings.Contains(result.stdout, secret) || strings.Contains(result.stderr, secret) {
			t.Fatal("export printed a secret value or unlock password")
		}
	}
}
