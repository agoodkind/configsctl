package main_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	ciRootName        = "root"
	ciWorkspace       = "qa"
	ciPassphraseName  = "TF_VAR_state_passphrase"
	ciPassphraseValue = "synthetic-ci-passphrase-0001"
	ciSettingName     = "TF_VAR_setting"
	ciSettingValue    = "visible-setting"
	ciProgressConfig  = `{"version": 1, "required_environment": ["TF_VAR_setting"], "secret_environment": ["TF_VAR_state_passphrase"], "secret_files": {}, "output_mode": "redacted_progress"}`
	ciEncryptedModule = `
variable "state_passphrase" {
  type      = string
  sensitive = true
}

variable "setting" {
  type = string
}

terraform {
  backend "local" {}

  encryption {
    key_provider "pbkdf2" "main" {
      passphrase = "${var.state_passphrase}"
    }
    method "aes_gcm" "main" {
      keys = key_provider.pbkdf2.main
    }
    state {
      method   = method.aes_gcm.main
      enforced = true
    }
    plan {
      method   = method.aes_gcm.main
      enforced = true
    }
  }
}

resource "terraform_data" "item" {
  input = var.setting
}

output "passphrase_echo" {
  value = nonsensitive(var.state_passphrase)
}
`
)

// ciCheckout is a checkout directory with one OpenTofu root and private HOME
// and TMPDIR directories.
type ciCheckout struct {
	dir    string
	home   string
	tmpdir string
}

func newCICheckout(t *testing.T) ciCheckout {
	t.Helper()
	checkout := ciCheckout{dir: t.TempDir(), home: t.TempDir(), tmpdir: t.TempDir()}
	for _, directory := range []string{checkout.home, checkout.tmpdir} {
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatalf("restrict %s: %v", directory, err)
		}
	}
	writeFixtureFile(t, filepath.Join(checkout.dir, ciRootName, "main.tf"), ciEncryptedModule)
	return checkout
}

func (c ciCheckout) environment() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + c.home,
		"TMPDIR=" + c.tmpdir,
		"TF_IN_AUTOMATION=1",
		ciPassphraseName + "=" + ciPassphraseValue,
		ciSettingName + "=" + ciSettingValue,
	}
}

func (c ciCheckout) writeConfig(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(c.dir, name)
	writeFixtureFile(t, path, content)
	return path
}

// run runs configsctl in the checkout with the fixture environment.
func (c ciCheckout) run(t *testing.T, args ...string) commandResult {
	t.Helper()
	command := exec.CommandContext(context.Background(), configsctlBinary, args...)
	command.Dir = c.dir
	command.Env = c.environment()
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()
	result := commandResult{stdout: stdout.String(), stderr: stderr.String(), exitCode: 0}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		result.exitCode = exitErr.ExitCode()
	default:
		t.Fatalf("run configsctl %v: %v", args, runErr)
	}
	return result
}

func (c ciCheckout) tofu(t *testing.T, config string, args ...string) commandResult {
	t.Helper()
	full := append([]string{"ci", "tofu", "--config", config, "--root", ciRootName, "--workspace", ciWorkspace, "--"}, args...)
	return c.run(t, full...)
}

func requireCIExit(t *testing.T, result commandResult, want int, what string) {
	t.Helper()
	if result.exitCode != want {
		t.Fatalf("%s exited with %d, want %d\nstdout:\n%s\nstderr:\n%s", what, result.exitCode, want, result.stdout, result.stderr)
	}
}

// prepare initializes the root, creates the workspace with tofu, selects it
// through ci tofu, and writes a saved plan.
func (c ciCheckout) prepare(t *testing.T, config string) string {
	t.Helper()
	requireCIExit(t, c.tofu(t, config, "init", "-input=false", "-lockfile=readonly", "-no-color"), 0, "ci tofu init")
	create := exec.CommandContext(context.Background(), "tofu", "workspace", "new", "-no-color", ciWorkspace)
	create.Dir = filepath.Join(c.dir, ciRootName)
	create.Env = c.environment()
	if output, err := create.CombinedOutput(); err != nil {
		t.Fatalf("tofu workspace new: %v\n%s", err, output)
	}
	requireCIExit(t, c.tofu(t, config, "workspace", "select", "-no-color", ciWorkspace), 0, "ci tofu workspace select")
	planPath := filepath.Join(c.tmpdir, "ci.tfplan")
	planned := c.tofu(t, config, "plan", "-input=false", "-no-color", "-detailed-exitcode", "-out="+planPath)
	requireCIExit(t, planned, 2, "ci tofu plan")
	if strings.Contains(planned.stdout, ciPassphraseValue) || !strings.Contains(planned.stdout, "<redacted:"+ciPassphraseName+">") {
		t.Fatalf("ci tofu plan did not redact the passphrase in stdout:\n%s", planned.stdout)
	}
	return planPath
}

func TestCITofuRejectsInvalidConfigurations(t *testing.T) {
	checkout := newCICheckout(t)
	cases := map[string]string{
		"duplicate field":           `{"version": 1, "version": 1, "required_environment": [], "secret_environment": [], "secret_files": {}, "output_mode": "redacted_progress"}`,
		"duplicate secret file":     `{"version": 1, "required_environment": [], "secret_environment": [], "secret_files": {"TF_VAR_a": "/a", "TF_VAR_a": "/b"}, "output_mode": "redacted_progress"}`,
		"unknown field":             `{"version": 1, "required_environment": [], "secret_environment": [], "secret_files": {}, "output_mode": "redacted_progress", "lock_bypass": true}`,
		"field with other case":     `{"Version": 1, "required_environment": [], "secret_environment": [], "secret_files": {}, "output_mode": "redacted_progress"}`,
		"missing field":             `{"version": 1, "required_environment": [], "secret_environment": [], "output_mode": "redacted_progress"}`,
		"null field":                `{"version": 1, "required_environment": null, "secret_environment": [], "secret_files": {}, "output_mode": "redacted_progress"}`,
		"unsupported version":       `{"version": 2, "required_environment": [], "secret_environment": [], "secret_files": {}, "output_mode": "redacted_progress"}`,
		"unsupported output mode":   `{"version": 1, "required_environment": [], "secret_environment": [], "secret_files": {}, "output_mode": "raw"}`,
		"private JSON without path": `{"version": 1, "required_environment": [], "secret_environment": [], "secret_files": {}, "output_mode": "private_json"}`,
		"path with progress output": `{"version": 1, "required_environment": [], "secret_environment": [], "secret_files": {}, "output_mode": "redacted_progress", "json_path": "/tmp/plan.json"}`,
		"trailing data":             `{"version": 1, "required_environment": [], "secret_environment": [], "secret_files": {}, "output_mode": "redacted_progress"} {}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			config := checkout.writeConfig(t, "config.json", content)
			result := checkout.tofu(t, config, "validate", "-no-color")
			requireCIExit(t, result, 1, "ci tofu with an invalid configuration")
			if !strings.Contains(result.stderr, "ci tofu configuration") {
				t.Fatalf("stderr does not report the configuration error:\n%s", result.stderr)
			}
			if _, err := os.Stat(filepath.Join(checkout.dir, ciRootName, ".terraform")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("OpenTofu ran with an invalid configuration: %v", err)
			}
		})
	}
}

func TestCITofuRejectsMutationCommands(t *testing.T) {
	checkout := newCICheckout(t)
	config := checkout.writeConfig(t, "config.json", ciProgressConfig)
	planPath := checkout.prepare(t, config)
	commands := map[string][]string{
		"apply":            {"apply", "-input=false", planPath},
		"auto apply":       {"apply", "-input=false", "-auto-approve"},
		"destroy":          {"destroy", "-auto-approve"},
		"import":           {"import", "terraform_data.item", "id"},
		"state removal":    {"state", "rm", "terraform_data.item"},
		"workspace new":    {"workspace", "new", "other"},
		"workspace create": {"workspace", "select", "-or-create", ciWorkspace},
		"force unlock":     {"force-unlock", "-force", "lock-id"},
		"refresh":          {"refresh"},
	}
	for name, args := range commands {
		t.Run(name, func(t *testing.T) {
			result := checkout.tofu(t, config, args...)
			requireCIExit(t, result, 1, "ci tofu "+name)
			if strings.Contains(result.stdout, "Tofu output streams to") {
				t.Fatalf("ci tofu %s started OpenTofu:\n%s", name, result.stdout)
			}
		})
	}
	state := filepath.Join(checkout.dir, ciRootName, "terraform.tfstate.d", ciWorkspace, "terraform.tfstate")
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused command wrote the state file %s: %v", state, err)
	}
}

func TestCITofuWritesPrivateJSON(t *testing.T) {
	checkout := newCICheckout(t)
	progress := checkout.writeConfig(t, "progress.json", ciProgressConfig)
	planPath := checkout.prepare(t, progress)
	jsonPath := filepath.Join(checkout.tmpdir, "plan.json")
	privateConfig := `{"version": 1, "required_environment": ["TF_VAR_setting"], "secret_environment": ["TF_VAR_state_passphrase"], "secret_files": {}, "output_mode": "private_json", "json_path": "` + jsonPath + `"}`
	config := checkout.writeConfig(t, "private.json", privateConfig)

	shown := checkout.tofu(t, config, "show", "-json", planPath)
	requireCIExit(t, shown, 0, "ci tofu show -json")
	if strings.Contains(shown.stdout, "resource_changes") || strings.Contains(shown.stdout, ciPassphraseValue) {
		t.Fatalf("ci tofu show -json wrote plan JSON to stdout:\n%s", shown.stdout)
	}
	info, err := os.Lstat(jsonPath)
	if err != nil {
		t.Fatalf("stat the JSON file: %v", err)
	}
	if mode := info.Mode(); !mode.IsRegular() || mode.Perm() != 0o600 {
		t.Fatalf("the JSON file has mode %v, want a regular file with mode 0600", mode)
	}
	written, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read the JSON file: %v", err)
	}
	direct := exec.CommandContext(context.Background(), "tofu", "show", "-json", planPath)
	direct.Dir = filepath.Join(checkout.dir, ciRootName)
	direct.Env = checkout.environment()
	want, err := direct.Output()
	if err != nil {
		t.Fatalf("tofu show -json: %v", err)
	}
	if !bytes.Equal(written, want) {
		t.Fatal("the JSON file differs from the output of tofu show -json")
	}
}

func TestCITofuRunsBeforeTheVaultIsRead(t *testing.T) {
	checkout := newCICheckout(t)
	writeFixtureFile(t, filepath.Join(checkout.home, ".config", "ansible", "vault.pass"), "fixture-phrase\n")
	writeFixtureFile(t, filepath.Join(checkout.dir, "ansible", "inventory", "group_vars", "all", "vault.yml"), "not a vault\n")
	requireCIExit(t, checkout.run(t, "version"), 1, "configsctl version with an unreadable vault")

	config := checkout.writeConfig(t, "config.json", ciProgressConfig)
	result := checkout.tofu(t, config, "init", "-input=false", "-lockfile=readonly", "-no-color")
	requireCIExit(t, result, 0, "ci tofu init with an unreadable vault")
}
