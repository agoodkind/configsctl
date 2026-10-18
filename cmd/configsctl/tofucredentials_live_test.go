package main_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ansiblevault "github.com/sosedoff/ansible-vault-go"
	"gopkg.in/yaml.v3"
)

// This integration test changes only the configured deployment account password.
// Its OpenTofu workspace uses the built-in terraform_data resource, without NIC jobs.
func TestDellCredentialSavedPlanLifecycle(t *testing.T) {
	root := os.Getenv("CONFIGSCTL_DELL_INTEGRATION_ROOT")
	if root == "" {
		t.Skip("CONFIGSCTL_DELL_INTEGRATION_ROOT must select an approved live Configs checkout")
	}
	requireTofu(t)
	data, err := os.ReadFile(filepath.Join(root, "configsctl.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var configuration map[string]yaml.Node
	if err := yaml.Unmarshal(data, &configuration); err != nil {
		t.Fatal(err)
	}
	var tofu map[string]yaml.Node
	tofuNode := configuration["tofu"]
	if err := tofuNode.Decode(&tofu); err != nil {
		t.Fatal(err)
	}
	var credentials map[string]yaml.Node
	credentialsNode := tofu["credentials"]
	if err := credentialsNode.Decode(&credentials); err != nil {
		t.Fatal(err)
	}
	credential, exists := credentials["opentofu/firmware"]
	if !exists {
		t.Fatal("opentofu/firmware credential adapter is missing")
	}
	credentialData, err := yaml.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	fixture := t.TempDir()
	settings := "tofu:\n  workspaces_dir: workspaces\n  env_key_prefix: vault_tofu_env_\n  lock_targets:\n    workspaces/firmware:\n      lock_free_types: [terraform_data]\n      module_hosts:\n        module.unused: poweredge\n  credentials:\n    workspaces/firmware:\n"
	for _, line := range strings.Split(strings.TrimSuffix(string(credentialData), "\n"), "\n") {
		settings += "      " + line + "\n"
	}
	writeFixtureFile(t, filepath.Join(fixture, "configsctl.yml"), settings)
	writeFixtureFile(t, filepath.Join(fixture, "ansible", "ansible.cfg"), lockedAnsibleConfig)
	writeFixtureFile(t, filepath.Join(fixture, "ansible", "inventory", "hosts.yml"), "all:\n  children:\n    proxmox_servers:\n      hosts:\n        poweredge:\n          ansible_host: 3d06:bad:b01:300::254\n          ansible_user: root\n")
	vaultDirectory := filepath.Join(fixture, "ansible", "inventory", "group_vars", "all")
	if err := os.MkdirAll(vaultDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "ansible", "inventory", "group_vars", "all", "vault.yml"), filepath.Join(vaultDirectory, "vault.yml")); err != nil {
		t.Fatal(err)
	}
	certificateDirectory := filepath.Join(fixture, "opentofu", "firmware")
	if err := os.MkdirAll(certificateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "opentofu", "firmware", "idrac-ca.pem"), filepath.Join(certificateDirectory, "idrac-ca.pem")); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(fixture, "workspaces", "firmware")
	backendHCL, err := os.ReadFile("testdata/dellcredential/backend.tf")
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(workspace, "backend.tf"), string(backendHCL))
	fixtureHCL, err := os.ReadFile("testdata/dellcredential/main.tf")
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(workspace, "main.tf"), string(fixtureHCL))
	run := func(args ...string) commandResult {
		command := exec.CommandContext(context.Background(), configsctlBinary, append([]string{"tofu", "firmware"}, args...)...)
		command.Dir = fixture
		command.Env = os.Environ()
		var output bytes.Buffer
		command.Stdout = &output
		command.Stderr = &output
		err := command.Run()
		result := commandResult{stdout: output.String()}
		if err != nil {
			result.exitCode = 1
		}
		return result
	}
	requireSuccess(t, run("init", "-input=false"))
	passwordPath, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := os.ReadFile(filepath.Join(passwordPath, ".config", "ansible", "vault.pass"))
	if err != nil {
		t.Fatal(err)
	}
	vaultPath := filepath.Join(vaultDirectory, "vault.yml")
	if err := os.Remove(vaultPath); err != nil {
		t.Fatal(err)
	}
	if err := ansiblevault.EncryptFile(vaultPath, "vault_poweredge_idrac_rotator_password: placeholder-invalid-login-0001\n", strings.TrimSpace(string(unlock))); err != nil {
		t.Fatal(err)
	}
	failedLogin := run("plan", "-input=false", "-out=failed-login.plan")
	if failedLogin.exitCode == 0 || !strings.Contains(failedLogin.stdout, "HTTP 401") {
		t.Fatal("invalid rotator login did not stop the CLI with its authentication error")
	}
	requireNoFile(t, filepath.Join(workspace, "failed-login.plan"))
	if err := os.Remove(vaultPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "ansible", "inventory", "group_vars", "all", "vault.yml"), vaultPath); err != nil {
		t.Fatal(err)
	}
	requireSuccess(t, run("plan", "-input=false", "-out=first.plan"))
	first := run("apply", "-input=false", "first.plan")
	requireSuccess(t, first)
	if !strings.Contains(first.stdout, "hostlock.acquired") {
		t.Fatal("apply did not acquire the required live host lock")
	}
	readback := run("output", "-raw", "active_flow_offload")
	requireSuccess(t, readback)
	if !strings.Contains(readback.stdout, "Enabled") {
		t.Fatal("the real provider did not read the active NIC attribute")
	}
	statePath := filepath.Join(workspace, "terraform.tfstate")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(state, []byte("poweredge_idrac_password")) || bytes.Contains(state, []byte("poweredge_idrac_user")) {
		t.Fatal("state contains an ephemeral credential variable")
	}
	stale := run("apply", "-input=false", "first.plan")
	if stale.exitCode == 0 {
		t.Fatal("the stale saved plan unexpectedly applied")
	}
	if !strings.Contains(stale.stdout, "Saved plan is stale") {
		t.Fatalf("OpenTofu did not report the stale saved plan: %s", stale.stdout)
	}
	requireSuccess(t, run("plan", "-input=false", "-out=recovery.plan"))
	requireSuccess(t, run("apply", "-input=false", "recovery.plan"))
	if err := os.WriteFile(filepath.Join(workspace, "main.tf"), []byte("invalid configuration"), 0o600); err != nil {
		t.Fatal(err)
	}
	failed := run("plan", "-input=false")
	if failed.exitCode == 0 {
		t.Fatal("invalid workspace unexpectedly planned")
	}
	if strings.Contains(failed.stdout, "hostlock.acquired") {
		t.Fatal("invalid credential declarations acquired a deployment lock")
	}
}
