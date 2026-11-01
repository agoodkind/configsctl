//go:build unix

package main_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// explicitSettings declares an ssh endpoint for every lock host of the
// workspace. The guest module key maps to guest-alpha, and the apply locks
// guest-alpha without locking hv_alpha.
const explicitSettings = `tofu:
  workspaces_dir: workspaces
  env_key_prefix: fixture_env_
  guest_modules:
    suburban/tack: module.base
  lock_targets:
    workspaces/suburban/tack:
      node_hosts:
        site: hv_alpha
      guest_hosts:
        site/101: guest-alpha
      module_key_hosts:
        module.base:
          guest_alpha: guest-alpha
      lock_endpoints:
        hv_alpha:
          address: hv-alpha.example
        guest-alpha:
          user: deploy
          address: guest-alpha.example
`

const (
	explicitWorkspace = "suburban/tack"
	explicitPlanFile  = "mutation.tfplan"
)

var ansibleProgramNames = []string{"ansible", "ansible-inventory", "ansible-playbook", "ansible-vault"}

// explicitFixture is a configuration tree with no Ansible directory and a
// directory of stand-in programs that stays first on PATH.
type explicitFixture struct {
	tree    configsTree
	binDir  string
	lockDir string
}

func newExplicitFixture(t *testing.T, settings string) explicitFixture {
	t.Helper()
	requireTofu(t)
	tree := newConfigsTree(t)
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), explicitSettings)
	root := filepath.Join(tree.workspaces, "suburban", "tack")
	writeFixtureFile(t, filepath.Join(root, "backend.tf"), fmt.Sprintf(localBackend, "tack.tfstate"))
	writeFixtureFile(t, filepath.Join(root, "main.tf"), workspaceFixture(t, "gate.tf"))
	writeFixtureFile(t, filepath.Join(root, "item", "main.tf"), gatedItemModule)
	args := tofuArgs(explicitWorkspace, "init", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, args...), args...)
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), settings)
	// The vault file stays under ansible/inventory/group_vars, which the Go vault
	// reader needs. The tree has neither an Ansible configuration nor a host inventory.
	for _, name := range []string{filepath.Join("ansible", "ansible.cfg"), filepath.Join("ansible", "inventory", "hosts.yml")} {
		if _, err := os.Stat(filepath.Join(tree.root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the fixture tree has %s, or Stat failed: %v", name, err)
		}
	}
	fixture := explicitFixture{tree: tree, binDir: t.TempDir(), lockDir: t.TempDir()}
	fixture.installStandIns(t)
	return fixture
}

func (f explicitFixture) installStandIns(t *testing.T) {
	t.Helper()
	realTofu, err := exec.LookPath("tofu")
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(realTofu)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(absolute, filepath.Join(f.binDir, "real-tofu")); err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{
		"tofu": filepath.Join("testdata", "explicit", "tofu"),
		"ssh":  filepath.Join("testdata", "signalstop", "ssh"),
	}
	for name, source := range sources {
		script, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("read the %s stand-in: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(f.binDir, name), script, 0o700); err != nil {
			t.Fatalf("install the %s stand-in: %v", name, err)
		}
	}
}

// apply runs "configsctl tofu suburban/tack apply" with a PATH that lists the
// stand-ins and the system programs, and lists no Ansible program.
func (f explicitFixture) apply(t *testing.T, args ...string) commandResult {
	t.Helper()
	path := f.binDir + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin"
	requireNoAnsibleProgram(t, path)
	tempDir := filepath.Join(f.tree.root, "tmp")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		t.Fatal(err)
	}
	commandArgs := tofuArgs(explicitWorkspace, append([]string{"apply"}, args...)...)
	command := exec.CommandContext(t.Context(), configsctlBinary, commandArgs...)
	command.Dir = f.tree.root
	command.Env = []string{
		"PATH=" + path, "HOME=" + f.tree.home, "TMPDIR=" + tempDir, "FIXTURE_LOCK_DIR=" + f.lockDir,
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()
	result := commandResult{stdout: stdout.String(), stderr: stderr.String(), exitCode: 0}
	var exited *exec.ExitError
	if errors.As(runErr, &exited) {
		result.exitCode = exited.ExitCode()
	} else if runErr != nil {
		t.Fatalf("run configsctl %v: %v", commandArgs, runErr)
	}
	appendFailedRunLogs(t, &result, tempDir)
	return result
}

func requireNoAnsibleProgram(t *testing.T, path string) {
	t.Helper()
	for _, directory := range filepath.SplitList(path) {
		for _, name := range ansibleProgramNames {
			if _, err := os.Stat(filepath.Join(directory, name)); err == nil {
				t.Skipf("%s contains %s; the test needs a PATH without Ansible programs", directory, name)
			}
		}
	}
}

func (f explicitFixture) calls(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(f.binDir, "calls.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(body)), "\n")
}

func requireNoExplicitDirectories(t *testing.T, tree configsTree) {
	t.Helper()
	leftovers, err := filepath.Glob(filepath.Join(tree.root, "tmp", "configsctl-explicit-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("TMPDIR has private mutation directories after configsctl exited: %v", leftovers)
	}
}

func (f explicitFixture) stateFile() string {
	return filepath.Join(f.tree.workspaces, "suburban", "tack", "tack.tfstate")
}

func TestTofuMutationUsesDeclaredLockEndpointsWithoutInventory(t *testing.T) {
	fixture := newExplicitFixture(t, explicitSettings)
	args := []string{"-auto-approve", "-input=false"}
	result := fixture.apply(t, args...)
	requireSuccess(t, result, args...)
	if !strings.Contains(result.stderr, "tofu.mutation.explicit_lock_hosts") || !strings.Contains(result.stderr, "hosts=[guest-alpha]") {
		t.Fatalf("the apply did not lock the declared host guest-alpha: %s", result.stderr)
	}
	if !strings.Contains(result.stderr, "hostlock.acquired") {
		t.Fatalf("the apply did not acquire its lock: %s", result.stderr)
	}
	if strings.Contains(result.stderr, "inventory") || strings.Contains(result.stderr, "tofu.lock.") {
		t.Fatalf("the apply used the inventory lock path: %s", result.stderr)
	}
	requireFile(t, fixture.stateFile())
	requireNoPlanCopies(t, fixture.tree)
	requireNoExplicitDirectories(t, fixture.tree)
	calls := fixture.calls(t)
	if len(calls) != 3 || !strings.HasPrefix(calls[0], "plan ") || !strings.HasPrefix(calls[1], "show -json ") || !strings.HasPrefix(calls[2], "apply ") {
		t.Fatalf("the controller ran %q, want plan, show, apply", calls)
	}
}

func TestTofuMutationRefusesUndeclaredEndpoint(t *testing.T) {
	missing := strings.Replace(explicitSettings, "        hv_alpha:\n          address: hv-alpha.example\n", "", 1)
	fixture := newExplicitFixture(t, missing)
	result := fixture.apply(t, "-auto-approve", "-input=false")
	if result.exitCode == 0 {
		t.Fatalf("the apply accepted a host without a declared endpoint: %s", result.stderr)
	}
	if !strings.Contains(result.stderr, "lock_endpoints does not declare host hv_alpha from node_hosts.site") {
		t.Fatalf("the refusal omitted the undeclared host: %s", result.stderr)
	}
	if calls := fixture.calls(t); len(calls) != 0 {
		t.Fatalf("OpenTofu started before the refusal: %q", calls)
	}
	if strings.Contains(result.stderr, "hostlock.acquired") {
		t.Fatalf("the refused apply took a lock: %s", result.stderr)
	}
	requireNoFile(t, fixture.stateFile())
}

func TestTofuMutationAppliesTheExactSavedPlan(t *testing.T) {
	t.Run("unchanged plan", func(t *testing.T) {
		fixture := newExplicitFixture(t, explicitSettings)
		args := []string{"-auto-approve", "-input=false"}
		requireSuccess(t, fixture.apply(t, args...), args...)
		planned, err := os.ReadFile(filepath.Join(fixture.binDir, "planned.tfplan"))
		if err != nil {
			t.Fatal(err)
		}
		applied, err := os.ReadFile(filepath.Join(fixture.binDir, "applied.tfplan"))
		if err != nil {
			t.Fatal(err)
		}
		if len(planned) == 0 || !bytes.Equal(planned, applied) {
			t.Fatal("the apply did not receive the bytes that the plan command wrote")
		}
		calls := fixture.calls(t)
		last := calls[len(calls)-1]
		if !strings.HasPrefix(last, "apply -input=false -no-color -parallelism=10 ") || !strings.HasSuffix(last, "/"+explicitPlanFile) {
			t.Fatalf("the apply ran %q, want one saved plan", last)
		}
		requireFile(t, fixture.stateFile())
	})
	t.Run("changed plan", func(t *testing.T) {
		fixture := newExplicitFixture(t, explicitSettings)
		if err := os.WriteFile(filepath.Join(fixture.binDir, "tamper"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		args := []string{"-auto-approve", "-input=false"}
		result := fixture.apply(t, args...)
		if result.exitCode == 0 || !strings.Contains(result.stderr, "changed after planning") {
			t.Fatalf("the apply did not refuse a changed plan: %s", result.stderr)
		}
		for _, call := range fixture.calls(t) {
			if strings.HasPrefix(call, "apply ") {
				t.Fatalf("OpenTofu applied a changed plan: %q", call)
			}
		}
		requireNoFile(t, fixture.stateFile())
		requireNoExplicitDirectories(t, fixture.tree)
	})
}
