package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	ansiblevault "github.com/sosedoff/ansible-vault-go"
)

const fixtureBackend = `
terraform {
  required_version = ">= 1.8"

  backend "local" {
    path = "fixture.tfstate"
  }
}
`

// writeWorkspaceTree creates a workspaces directory with a backend block and
// four child directories. "alpha" is a workspace in native syntax. "beta" is a
// workspace in JSON syntax. "shared" is a module with a terraform block and no
// backend block. "empty" has no files.
func writeWorkspaceTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "workspaces")
	for _, name := range []string{"alpha", "beta", "shared", "empty"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	writeTestFile(t, filepath.Join(root, "backend.tf"), fixtureBackend)
	rootVariables := "variable \"vault_root\" {\n  type = string\n}\n"
	writeTestFile(t, filepath.Join(root, "variables.tf"), rootVariables)

	writeTestFile(t, filepath.Join(root, "alpha", "backend.tf"), fixtureBackend)
	alphaVariables := `
variable "vault_alpha" {
  type      = string
  sensitive = true
}

variable "plain_setting" {
  type    = string
  default = "value"
}
`
	writeTestFile(t, filepath.Join(root, "alpha", "variables.tf"), alphaVariables)
	alphaJSON := `{"variable": {"vault_delta": {"type": "string"}, "fixture_env_BOTH": {"type": "string"}}}`
	writeTestFile(t, filepath.Join(root, "alpha", "extra.tf.json"), alphaJSON)

	betaJSON := `{"terraform": {"backend": {"local": {"path": "beta.tfstate"}}},` +
		` "variable": {"vault_beta": {"type": "string"}}}`
	writeTestFile(t, filepath.Join(root, "beta", "main.tf.json"), betaJSON)

	sharedModule := `
terraform {
  required_version = ">= 1.8"
}

variable "vault_shared" {
  type = string
}
`
	writeTestFile(t, filepath.Join(root, "shared", "main.tf"), sharedModule)
	return root
}

func TestTofuChildWorkspaces(t *testing.T) {
	root := writeWorkspaceTree(t)
	got, err := tofuChildWorkspaces(root)
	if err != nil {
		t.Fatalf("tofuChildWorkspaces: %v", err)
	}
	want := []string{"alpha", "beta"}
	if !slices.Equal(got, want) {
		t.Fatalf("tofuChildWorkspaces = %v, want %v", got, want)
	}
}

func TestSelectTofuWorkspace(t *testing.T) {
	root := writeWorkspaceTree(t)
	tests := []struct {
		name     string
		args     []string
		wantDir  string
		wantArgs []string
	}{
		{
			name:     "child workspace name",
			args:     []string{"alpha", "plan", "-detailed-exitcode"},
			wantDir:  filepath.Join(root, "alpha"),
			wantArgs: []string{"plan", "-detailed-exitcode"},
		},
		{
			name:     "json child workspace name",
			args:     []string{"beta", "init"},
			wantDir:  filepath.Join(root, "beta"),
			wantArgs: []string{"init"},
		},
		{
			name:     "no workspace name",
			args:     []string{"plan"},
			wantDir:  root,
			wantArgs: []string{"plan"},
		},
		{
			name:     "module directory name",
			args:     []string{"shared", "plan"},
			wantDir:  root,
			wantArgs: []string{"shared", "plan"},
		},
		{
			name:     "workspace name after the first argument",
			args:     []string{"plan", "alpha"},
			wantDir:  root,
			wantArgs: []string{"plan", "alpha"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotDir, gotArgs, err := selectTofuWorkspace(root, tc.args)
			if err != nil {
				t.Fatalf("selectTofuWorkspace: %v", err)
			}
			if gotDir != tc.wantDir {
				t.Fatalf("directory = %q, want %q", gotDir, tc.wantDir)
			}
			if !slices.Equal(gotArgs, tc.wantArgs) {
				t.Fatalf("arguments = %v, want %v", gotArgs, tc.wantArgs)
			}
		})
	}
}

func TestSelectTofuWorkspaceWithoutRootBackend(t *testing.T) {
	root := writeWorkspaceTree(t)
	if err := os.Remove(filepath.Join(root, "backend.tf")); err != nil {
		t.Fatalf("remove root backend: %v", err)
	}

	gotDir, gotArgs, err := selectTofuWorkspace(root, []string{"alpha", "plan"})
	if err != nil {
		t.Fatalf("selectTofuWorkspace with a workspace name: %v", err)
	}
	if gotDir != filepath.Join(root, "alpha") || !slices.Equal(gotArgs, []string{"plan"}) {
		t.Fatalf("selection = %q %v, want the alpha directory and [plan]", gotDir, gotArgs)
	}

	_, _, err = selectTofuWorkspace(root, []string{"plan"})
	if err == nil {
		t.Fatal("selectTofuWorkspace accepted a workspaces directory without a backend block")
	}
	if !strings.Contains(err.Error(), "alpha, beta") {
		t.Fatalf("error = %q, want it to list the workspaces alpha, beta", err)
	}
	if strings.Contains(err.Error(), "shared") {
		t.Fatalf("error = %q, want it to omit the module directory shared", err)
	}

	_, _, err = selectTofuWorkspace(filepath.Join(root, "empty"), []string{"plan"})
	if err == nil {
		t.Fatal("selectTofuWorkspace accepted a directory with no workspace")
	}
}

func TestTofuSecretEnvPerWorkspace(t *testing.T) {
	root := writeWorkspaceTree(t)
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "configsctl.yml")
	settingsContent := "tofu:\n  workspaces_dir: " + root + "\n  env_key_prefix: fixture_env_\n"
	writeTestFile(t, settingsPath, settingsContent)
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
		"vault_root: seven\nvault_shared: eight\n"
	if err := ansiblevault.EncryptFile(vaultFile, vaultContent, vaultPhrase); err != nil {
		t.Fatalf("encrypt vault: %v", err)
	}

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "native workspace",
			args: []string{"alpha", "plan"},
			want: []string{
				"TF_VAR_fixture_env_BOTH=six",
				"TF_VAR_vault_alpha=one",
				"TF_VAR_vault_delta=five",
				"FIXTURE_NAME=four",
			},
		},
		{
			name: "json workspace",
			args: []string{"beta", "plan"},
			want: []string{"TF_VAR_vault_beta=two", "BOTH=six", "FIXTURE_NAME=four"},
		},
		{
			name: "workspaces directory",
			args: []string{"plan"},
			want: []string{"TF_VAR_vault_root=seven", "BOTH=six", "FIXTURE_NAME=four"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			workspaceDir, _, err := selectTofuWorkspace(loaded.Tofu.WorkspacesDir, tc.args)
			if err != nil {
				t.Fatalf("selectTofuWorkspace: %v", err)
			}
			got, err := tofuSecretEnv(loaded.Tofu, workspaceDir, vaultFile, passwordFile)
			if err != nil {
				t.Fatalf("tofuSecretEnv: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("tofuSecretEnv = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoadSettingsRejectsIncompleteFile(t *testing.T) {
	cases := map[string]string{
		"missing prefix":     "tofu:\n  workspaces_dir: opentofu\n",
		"missing workspaces": "tofu:\n  env_key_prefix: fixture_env_\n",
		"unknown key":        "tofu:\n  workspaces_dir: opentofu\n  env_key_prefix: fixture_env_\n  extra: 1\n",
		"removed module key": "tofu:\n  module_dir: opentofu\n  env_key_prefix: fixture_env_\n",
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
