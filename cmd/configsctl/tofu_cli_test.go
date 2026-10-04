package main_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	ansiblevault "github.com/sosedoff/ansible-vault-go"
)

// configsctlBinary is the command built once by TestMain.
var configsctlBinary string

func TestMain(m *testing.M) {
	os.Exit(runWithBuiltBinary(m))
}

func runWithBuiltBinary(m *testing.M) int {
	buildDir, err := os.MkdirTemp("", "configsctl-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create build directory: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(buildDir) }()

	configsctlBinary = filepath.Join(buildDir, "configsctl")
	build := exec.CommandContext(context.Background(), "go", "build", "-o", configsctlBinary, ".")
	output, err := build.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build configsctl: %v\n%s", err, output)
		return 1
	}
	return m.Run()
}

// Every vault value has at least 16 characters, the redaction minimum. Every
// value has a distinct length. A test reads the length of a variable to identify
// its value.
const (
	vaultUnlockText    = "fixture-phrase"
	vaultAlphaValue    = "alpha-secret-value-001"
	vaultBetaValue     = "beta-secret-value-00000002"
	vaultRootValue     = "root-secret-value-000000000003"
	vaultPrefixedValue = "prefixed-secret-value-0000000000004"
	vaultOtherValue    = "unrelated-secret-value-00000000000005"
	plainSettingValue  = "value"
)

const localBackend = `
terraform {
  backend "local" {
    path = "%s"
  }
}
`

const rootWorkspace = `
variable "vault_root" {
  type = string
}

output "root_length" {
  value = length(var.vault_root)
}
`

const alphaWorkspace = `
variable "vault_alpha" {
  type = string
}

variable "prefixed" {
  type    = string
  default = ""
}

variable "plain_setting" {
  type    = string
  default = "value"
}

output "alpha_length" {
  value = length(var.vault_alpha)
}

output "prefixed_length" {
  value = length(var.prefixed)
}

output "plain_length" {
  value = length(var.plain_setting)
}
`

const betaWorkspaceJSON = `{
  "terraform": {"backend": {"local": {"path": "beta.tfstate"}}},
  "variable": {"vault_beta": {"type": "string"}},
  "output": {"beta_length": {"value": "${length(var.vault_beta)}"}}
}`

const sharedModule = `
variable "vault_shared" {
  type    = string
  default = ""
}
`

const settingsContent = "tofu:\n  workspaces_dir: workspaces\n  env_key_prefix: fixture_env_\n"

// configsTree is a temporary configs repository with a separate home directory
// for the vault password file.
type configsTree struct {
	root       string
	home       string
	workspaces string
}

type commandResult struct {
	stdout   string
	stderr   string
	exitCode int
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newConfigsTree writes a workspaces directory with a root backend, the child
// workspaces alpha (native syntax) and beta (JSON syntax), and the module
// directory shared, which has no backend block.
func newConfigsTree(t *testing.T) configsTree {
	t.Helper()
	base := t.TempDir()
	tree := configsTree{
		root:       filepath.Join(base, "configs"),
		home:       filepath.Join(base, "home"),
		workspaces: filepath.Join(base, "configs", "workspaces"),
	}
	writeFixtureFile(t, filepath.Join(tree.root, "configsctl.yml"), settingsContent)

	writeFixtureFile(t, filepath.Join(tree.workspaces, "backend.tf"), fmt.Sprintf(localBackend, "root.tfstate"))
	writeFixtureFile(t, filepath.Join(tree.workspaces, "main.tf"), rootWorkspace)
	writeFixtureFile(t, filepath.Join(tree.workspaces, "alpha", "backend.tf"), fmt.Sprintf(localBackend, "alpha.tfstate"))
	writeFixtureFile(t, filepath.Join(tree.workspaces, "alpha", "main.tf"), alphaWorkspace)
	writeFixtureFile(t, filepath.Join(tree.workspaces, "beta", "main.tf.json"), betaWorkspaceJSON)
	writeFixtureFile(t, filepath.Join(tree.workspaces, "shared", "main.tf"), sharedModule)

	passwordFile := filepath.Join(tree.home, ".config", "ansible", "vault.pass")
	writeFixtureFile(t, passwordFile, vaultUnlockText+"\n")

	vaultContent := "vault_alpha: " + vaultAlphaValue + "\n" +
		"vault_beta: " + vaultBetaValue + "\n" +
		"vault_root: " + vaultRootValue + "\n" +
		"fixture_env_TF_VAR_prefixed: " + vaultPrefixedValue + "\n" +
		"vault_unrelated: " + vaultOtherValue + "\n"
	vaultFile := filepath.Join(tree.root, "ansible", "inventory", "group_vars", "all", "vault.yml")
	if err := os.MkdirAll(filepath.Dir(vaultFile), 0o700); err != nil {
		t.Fatalf("create vault directory: %v", err)
	}
	if err := ansiblevault.EncryptFile(vaultFile, vaultContent, vaultUnlockText); err != nil {
		t.Fatalf("encrypt vault: %v", err)
	}
	return tree
}

// runConfigsctl runs the built command from the tree root. TMPDIR is a directory
// inside the tree for the run logs.
func runConfigsctl(t *testing.T, tree configsTree, args ...string) commandResult {
	t.Helper()
	tempDir := filepath.Join(tree.root, "tmp")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		t.Fatalf("create TMPDIR: %v", err)
	}
	command := exec.CommandContext(context.Background(), configsctlBinary, args...)
	command.Dir = tree.root
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + tree.home,
		"TMPDIR=" + tempDir,
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()
	result := commandResult{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		result.exitCode = 0
	case errors.As(runErr, &exitErr):
		result.exitCode = exitErr.ExitCode()
	default:
		t.Fatalf("run configsctl %v: %v", args, runErr)
	}
	return result
}

func requireTofu(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tofu"); err != nil {
		t.Skip("tofu is not installed")
	}
}

func requireSuccess(t *testing.T, result commandResult, args ...string) {
	t.Helper()
	if result.exitCode != 0 {
		t.Fatalf("configsctl %v exited %d\nstdout: %s\nstderr: %s",
			args, result.exitCode, result.stdout, result.stderr)
	}
}

// tofuArgs builds the arguments of one configsctl tofu call. The workspace is
// empty to select the workspaces directory.
func tofuArgs(workspace string, tofuCommand ...string) []string {
	args := []string{"tofu"}
	if workspace != "" {
		args = append(args, workspace)
	}
	return append(args, tofuCommand...)
}

// applyWorkspace runs init and apply in the workspace.
func applyWorkspace(t *testing.T, tree configsTree, workspace string) {
	t.Helper()
	initArgs := tofuArgs(workspace, "init", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, initArgs...), initArgs...)
	applyArgs := tofuArgs(workspace, "apply", "-auto-approve", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, applyArgs...), applyArgs...)
}

func readOutput(t *testing.T, tree configsTree, workspace, name string) string {
	t.Helper()
	args := tofuArgs(workspace, "output", "-raw", name)
	result := runConfigsctl(t, tree, args...)
	requireSuccess(t, result, args...)
	return result.stdout
}

func requireOutputLength(t *testing.T, tree configsTree, workspace, name, value string) {
	t.Helper()
	want := strconv.Itoa(len(value))
	if got := readOutput(t, tree, workspace, name); got != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}

func requireFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("want %s to exist: %v", path, err)
	}
}

func requireNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("want %s to be absent", path)
	}
}

func TestTofuRunsInTheSelectedWorkspace(t *testing.T) {
	requireTofu(t)
	tree := newConfigsTree(t)

	applyWorkspace(t, tree, "alpha")
	requireFile(t, filepath.Join(tree.workspaces, "alpha", "alpha.tfstate"))
	requireNoFile(t, filepath.Join(tree.workspaces, "root.tfstate"))

	applyWorkspace(t, tree, "beta")
	requireFile(t, filepath.Join(tree.workspaces, "beta", "beta.tfstate"))

	applyWorkspace(t, tree, "")
	requireFile(t, filepath.Join(tree.workspaces, "root.tfstate"))
}

func TestTofuPassesAModuleDirectoryNameToTofu(t *testing.T) {
	requireTofu(t)
	tree := newConfigsTree(t)

	result := runConfigsctl(t, tree, "tofu", "shared", "plan")
	if result.exitCode == 0 {
		t.Fatalf("configsctl tofu shared plan succeeded, want tofu to reject the argument\nstdout: %s", result.stdout)
	}
}

func TestTofuExportsVaultValuesForDeclaredVariables(t *testing.T) {
	requireTofu(t)
	tree := newConfigsTree(t)

	applyWorkspace(t, tree, "alpha")
	requireOutputLength(t, tree, "alpha", "alpha_length", vaultAlphaValue)
	requireOutputLength(t, tree, "alpha", "plain_length", plainSettingValue)

	applyWorkspace(t, tree, "beta")
	requireOutputLength(t, tree, "beta", "beta_length", vaultBetaValue)

	applyWorkspace(t, tree, "")
	requireOutputLength(t, tree, "", "root_length", vaultRootValue)
}

func TestTofuExportsVaultKeysWithTheEnvironmentPrefix(t *testing.T) {
	requireTofu(t)
	tree := newConfigsTree(t)

	applyWorkspace(t, tree, "alpha")
	requireOutputLength(t, tree, "alpha", "prefixed_length", vaultPrefixedValue)
}

func TestTofuWithoutRootBackendListsChildWorkspaces(t *testing.T) {
	tree := newConfigsTree(t)
	if err := os.Remove(filepath.Join(tree.workspaces, "backend.tf")); err != nil {
		t.Fatalf("remove root backend: %v", err)
	}

	result := runConfigsctl(t, tree, "tofu", "plan")
	if result.exitCode == 0 {
		t.Fatal("configsctl tofu plan succeeded without a root backend or a workspace name")
	}
	if !strings.Contains(result.stderr, "alpha, beta") {
		t.Fatalf("stderr = %q, want it to list the workspaces alpha, beta", result.stderr)
	}
	if strings.Contains(result.stderr, "shared") {
		t.Fatalf("stderr = %q, want it to omit the module directory", result.stderr)
	}
}

func TestTofuRejectsInvalidSettingsFile(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		fragment string
	}{
		{
			name:     "missing prefix",
			content:  "tofu:\n  workspaces_dir: workspaces\n",
			fragment: "env_key_prefix",
		},
		{
			name:     "missing workspaces directory",
			content:  "tofu:\n  env_key_prefix: fixture_env_\n",
			fragment: "workspaces_dir",
		},
		{
			name:     "unknown key",
			content:  settingsContent + "  extra: 1\n",
			fragment: "extra",
		},
		{
			name:     "removed module key",
			content:  "tofu:\n  module_dir: workspaces\n  env_key_prefix: fixture_env_\n",
			fragment: "module_dir",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			tree := newConfigsTree(t)
			writeFixtureFile(t, filepath.Join(tree.root, "configsctl.yml"), testCase.content)

			result := runConfigsctl(t, tree, "tofu", "plan")
			if result.exitCode == 0 {
				t.Fatalf("configsctl tofu plan accepted %q", testCase.content)
			}
			if !strings.Contains(result.stderr, testCase.fragment) {
				t.Fatalf("stderr = %q, want it to mention %q", result.stderr, testCase.fragment)
			}
		})
	}
}
