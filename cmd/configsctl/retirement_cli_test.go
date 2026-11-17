package main_test

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func retirementRun(t *testing.T, dir, program string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), program, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s failed: %v", program, args[0], err)
	}

	return strings.TrimSpace(string(out))
}

func retirementSave(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func retirementInstall(t *testing.T, dir, destination, fixture string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "retirement", fixture))
	if err != nil {
		t.Fatal(err)
	}

	retirementSave(t, filepath.Join(dir, destination), data)
}

func retirementCommit(t *testing.T, repo string) string {
	t.Helper()
	retirementRun(t, repo, "git", "add", "-A")
	retirementRun(
		t,
		repo,
		"git",
		"-c",
		"user.name=Test",
		"-c",
		"user.email=test@example.invalid",
		"-c",
		"commit.gpgsign=false",
		"commit",
		"-qm",
		"fixture",
	)

	return retirementRun(t, repo, "git", "rev-parse", "HEAD")
}

func retirementDrop(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func TestRetirement(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	repo := t.TempDir()
	root := filepath.Join(repo, "opentofu")
	second := filepath.Join(root, "second")
	pass := rand.Text()
	const variable = "TF_VAR_vault_tofu_state_passphrase"
	t.Setenv(variable, pass)
	t.Setenv("TF_VAR_fixture_value", "fixture")
	t.Setenv("TF_WORKSPACE", "")
	t.Setenv("TF_DATA_DIR", "")
	retirementRun(t, repo, "git", "init", "-q")
	retirementSave(t, filepath.Join(repo, ".gitignore"), []byte(".terraform/\n*.tfstate*\n"))
	retirementSave(
		t,
		filepath.Join(repo, "configsctl.yml"),
		[]byte("tofu:\n  workspaces_dir: opentofu\n"),
	)
	retirementBackend(t, root)
	retirementBackend(t, second)
	retirementInstall(t, root, "main.tf", "main.tf")
	retirementInstall(t, second, "main.tf", "second.tf")
	shared := filepath.Join(root, "modules", "shared")
	retirementInstall(t, shared, "records.tf", "records.tf")

	for _, dir := range []string{root, second} {
		retirementRun(t, dir, "tofu", "init", "-input=false", "-no-color")
		retirementRun(t, dir, "tofu", "apply", "-auto-approve", "-input=false")
	}

	retirementRun(
		t,
		root,
		"tofu",
		"state",
		"replace-provider",
		"-auto-approve",
		"terraform.io/builtin/terraform",
		"tofu.home.arpa/example/pveguest",
	)
	if !strings.Contains(
		retirementRun(t, root, "tofu", "state", "pull"),
		"tofu.home.arpa/example/pveguest",
	) {
		t.Fatal("the local state does not reference the unavailable provider")
	}

	retirementRun(t, second, "tofu", "workspace", "new", "review")
	retirementRun(t, second, "tofu", "apply", "-auto-approve", "-input=false")
	statePath := filepath.Join(second, "terraform.tfstate.d", "review", "terraform.tfstate")
	encrypted, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Contains(encrypted, []byte("encrypted_data")) ||
		bytes.Contains(encrypted, []byte("private-record-value")) {
		t.Fatal("the fixture did not encrypt the state")
	}

	retirementInstall(t, shared, "retire.tofu.json", "retire.tofu.json")
	retirementSave(t, filepath.Join(shared, "retire.tf.json"), []byte("invalid inactive JSON"))
	base := retirementCommit(t, repo)
	// Select a different backend while retaining the same removed instructions.
	retirementDrop(t, filepath.Join(second, "backend.tf"))
	retirementInstall(t, second, "backend.tf.json", "redirect.tf.json")
	redirected := retirementCommit(t, repo)
	if err := verifyRetirement(t, repo, base, redirected); err == nil ||
		!strings.Contains(err.Error(), `workspace \"review\" still contains a retired address`) {
		t.Fatalf("changed backend did not inspect its former named state: %v", err)
	}

	retirementDrop(t, filepath.Join(second, "backend.tf.json"))
	retirementBackend(t, second)
	retirementInstall(t, second, "module-removed.tf", "module.tf")
	moduleBase := retirementCommit(t, repo)
	retirementDrop(t, filepath.Join(second, "module-removed.tf"))
	moduleHead := retirementCommit(t, repo)
	if err := verifyRetirement(t, repo, moduleBase, moduleHead); err == nil ||
		!strings.Contains(err.Error(), `workspace \"default\" still contains a retired address`) {
		t.Fatalf("missing module check for indexed descendants: %v", err)
	}

	retirementDrop(t, filepath.Join(shared, "retire.tofu.json"))
	retirementDrop(t, filepath.Join(shared, "retire.tf.json"))
	retirementInstall(t, shared, "renamed.tf", "moved.tf")
	moved := retirementCommit(t, repo)
	t.Setenv(variable, "")
	if err := verifyRetirement(t, repo, base, moved); err != nil {
		t.Fatal(err)
	}

	retirementDrop(t, filepath.Join(shared, "renamed.tf"))
	retirementInstall(t, root, "poison.tf", "poison.tf")
	retirementDrop(t, filepath.Join(second, "backend.tf"))
	retirementDrop(t, filepath.Join(second, "main.tf"))
	head := retirementCommit(t, repo)
	t.Setenv(variable, pass)
	check := func(want string) {
		t.Helper()
		err := verifyRetirement(t, repo, base, head)
		if want == "" {
			if err != nil {
				t.Fatal(err)
			}

			return
		}

		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q, got %v", want, err)
		}

		if strings.Contains(err.Error(), pass) ||
			strings.Contains(err.Error(), "private-record-value") {
			t.Fatal("verification disclosed a state value")
		}
	}
	check(`workspace \"review\" still contains a retired address`)
	actual, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(actual, encrypted) {
		t.Fatal("verification changed named state")
	}

	retirementBackend(t, second)
	retirementInstall(t, second, "main.tf", "second.tf")
	retirementRun(t, second, "tofu", "state", "rm", "module.other[0].terraform_data.old[0]")
	beforeSuccess, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	check("")
	afterSuccess, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(beforeSuccess, afterSuccess) {
		t.Fatal("successful verification changed named state")
	}

	if os.Getuid() != 0 {
		if err := os.Chmod(statePath, 0); err != nil {
			t.Fatal(err)
		}

		check("requires an existing readable state")
		if err := os.Chmod(statePath, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv(variable, rand.Text())
	check("initialize and enumerate the base backend")
	t.Setenv(variable, pass)
	retirementDrop(t, statePath)
	check("requires an existing readable state")
	retirementDrop(t, filepath.Join(root, "runtime.tfstate"))
	check("requires an existing readable state")
}

func verifyRetirement(t *testing.T, repo, base, head string) error {
	t.Helper()
	cmd := exec.CommandContext(
		t.Context(),
		configsctlBinary,
		"ci",
		"retirement",
		"--base",
		base,
		"--head",
		head,
	)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	pass := os.Getenv("TF_VAR_vault_tofu_state_passphrase")
	if (pass != "" && bytes.Contains(out, []byte(pass))) ||
		bytes.Contains(out, []byte("private-record-value")) {
		t.Fatal("the command disclosed a private value")
	}

	if err != nil {
		return fmt.Errorf("retirement command: %w: %s", err, out)
	}

	return nil
}

func retirementBackend(t *testing.T, dir string) {
	t.Helper()
	data, err := os.ReadFile(
		filepath.Join("..", "..", "internal", "statemigration", "testdata", "retired.tf"),
	)
	if err != nil {
		t.Fatal(err)
	}

	retirementSave(t, filepath.Join(dir, "backend.tf"), data)
}
