package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTofuCredentialRejectsPersistentVariablesBeforeAuthentication(t *testing.T) {
	tree := newConfigsTree(t)
	writeFixtureFile(t, filepath.Join(tree.root, "configsctl.yml"), settingsContent+readCredentialFixture(t, "credentials.yml"))
	writeFixtureFile(t, filepath.Join(tree.workspaces, "alpha", "credentials.tf"), readCredentialFixture(t, "user.tf"))
	result := runConfigsctl(t, tree, "tofu", "alpha", "plan", "-input=false")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "credential variable vault_alpha requires ephemeral = true and sensitive = true") {
		t.Fatalf("the CLI did not reject the persistent password declaration: %s", result.stderr)
	}
	if strings.Contains(result.stderr, "hostlock.acquired") || strings.Contains(result.stderr, "Dell credential GET") {
		t.Fatal("the CLI started authentication before validating credential storage")
	}
}

func TestTofuCredentialRequiresItsHostLockBeforeAuthentication(t *testing.T) {
	tree := newConfigsTree(t)
	writeFixtureFile(t, filepath.Join(tree.root, "configsctl.yml"), settingsContent+readCredentialFixture(t, "credentials.yml"))
	writeFixtureFile(t, filepath.Join(tree.workspaces, "alpha", "main.tf"), readCredentialFixture(t, "credentials.tf"))
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "ansible.cfg"), lockedAnsibleConfig)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "inventory", "hosts.yml"), lockedInventory)
	result := runConfigsctl(t, tree, "tofu", "alpha", "plan", "-input=false")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "host hv_alpha from tofu.lock_targets has no ssh lock target") {
		t.Fatalf("the CLI did not require the credential host lock: %s", result.stderr)
	}
	if strings.Contains(result.stderr, "iDRAC credential GET") {
		t.Fatal("the CLI started authentication without its host lock")
	}
}

func readCredentialFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "dellcredential", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
