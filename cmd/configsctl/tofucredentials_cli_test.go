package main_test

import (
	"path/filepath"
	"strings"
	"testing"
)

const credentialSettings = `  credentials:
    workspaces/alpha:
      lock_host: hv_alpha
      username_variable: credential_user
      password_variable: vault_alpha
      dell:
        endpoint: https://[::1]
        rotator_user: rotator
        rotator_password_key: vault_beta
        account_id: "3"
        username: deployment
        read_paths: [/redfish/v1/Systems/System.Embedded.1]
`

func TestTofuCredentialRejectsPersistentVariablesBeforeAuthentication(t *testing.T) {
	tree := newConfigsTree(t)
	writeFixtureFile(t, filepath.Join(tree.root, "configsctl.yml"), settingsContent+credentialSettings)
	writeFixtureFile(t, filepath.Join(tree.workspaces, "alpha", "credentials.tf"), "variable \"credential_user\" {\n type = string\n ephemeral = true\n sensitive = true\n}\n")
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
	writeFixtureFile(t, filepath.Join(tree.root, "configsctl.yml"), settingsContent+credentialSettings)
	writeFixtureFile(t, filepath.Join(tree.workspaces, "alpha", "main.tf"), `variable "credential_user" {
  type = string
  ephemeral = true
  sensitive = true
}
variable "vault_alpha" {
  type = string
  ephemeral = true
  sensitive = true
}
`)
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
