//go:build unix

package main_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"goodkind.io/configsctl/hostlock"
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

var ansibleProgramNames = []string{
	"ansible",
	"ansible-inventory",
	"ansible-playbook",
	"ansible-vault",
}

// explicitFixture is a configuration tree with no Ansible directory and a
// directory of stand-in programs that stays first on PATH.
type explicitFixture struct {
	tree    configsTree
	binDir  string
	lockDir string
	// extraEnv adds variables to the configsctl process. stdin feeds its standard input.
	extraEnv []string
	stdin    string
	// observed installs the tofu wrapper that records calls and plan bytes.
	// Without it, configsctl runs the real tofu directly.
	observed bool
}

// newExplicitFixture returns a fixture with the observing tofu wrapper.
func newExplicitFixture(t *testing.T, settings string) explicitFixture {
	t.Helper()
	return newFixture(t, settings, true)
}

// newDirectFixture returns a fixture that runs the real tofu without the wrapper.
func newDirectFixture(t *testing.T, settings string) explicitFixture {
	t.Helper()
	return newFixture(t, settings, false)
}

// The explicit path tests are controller tests with a substituted ssh program.
// They do not prove that a declared endpoint owns the lock or that the lock path
// and the release happen on the remote host.
func newFixture(t *testing.T, settings string, observed bool) explicitFixture {
	t.Helper()
	requireTofu(t)

	return newFixtureWithTree(t, settings, observed, newConfigsTree(t))
}

func newFixtureWithTree(
	t *testing.T,
	settings string,
	observed bool,
	tree configsTree,
) explicitFixture {
	t.Helper()
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), explicitSettings)
	root := filepath.Join(tree.workspaces, "suburban", "tack")
	writeFixtureFile(
		t,
		filepath.Join(root, "backend.tf"),
		fmt.Sprintf(localBackend, "tack.tfstate"),
	)
	writeFixtureFile(t, filepath.Join(root, "main.tf"), workspaceFixture(t, "gate.tf"))
	writeFixtureFile(t, filepath.Join(root, "item", "main.tf"), gatedItemModule)
	args := tofuArgs(explicitWorkspace, "init", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, args...), args...)
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), settings)
	// The vault file stays under ansible/inventory/group_vars, which the Go vault
	// reader needs. The tree has neither an Ansible configuration nor a host inventory.
	for _, name := range []string{
		filepath.Join("ansible", "ansible.cfg"),
		filepath.Join("ansible", "inventory", "hosts.yml"),
	} {
		if _, err := os.Stat(filepath.Join(tree.root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the fixture tree has %s, or Stat failed: %v", name, err)
		}
	}

	fixture := explicitFixture{
		tree:     tree,
		binDir:   t.TempDir(),
		lockDir:  t.TempDir(),
		extraEnv: nil,
		stdin:    "",
		observed: observed,
	}
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

	sources := map[string]string{"ssh": filepath.Join("testdata", "signalstop", "ssh")}
	if f.observed {
		if err := os.Symlink(absolute, filepath.Join(f.binDir, "real-tofu")); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(
			filepath.Join(f.binDir, "lockdir"),
			[]byte(f.lockDir),
			0o600,
		); err != nil {
			t.Fatal(err)
		}

		sources["tofu"] = filepath.Join("testdata", "explicit", "tofu")
	} else if err := os.Symlink(absolute, filepath.Join(f.binDir, "tofu")); err != nil {
		t.Fatal(err)
	}

	for _, name := range ansibleProgramNames {
		sources[name] = filepath.Join("testdata", "explicit", "ansible")
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
// stand-ins, including the Ansible sentinels, before the system programs.
func (f explicitFixture) apply(t *testing.T, args ...string) commandResult {
	t.Helper()
	return f.mutate(t, append([]string{"apply"}, args...)...)
}

// mutate runs "configsctl tofu suburban/tack" with the given OpenTofu arguments.
func (f explicitFixture) mutate(t *testing.T, args ...string) commandResult {
	t.Helper()
	return f.mutateWorkspace(t, explicitWorkspace, args...)
}

func (f explicitFixture) mutateWorkspace(
	t *testing.T,
	workspace string,
	args ...string,
) commandResult {
	t.Helper()
	publishConfigsTree(t, f.tree)
	path := f.binDir + string(os.PathListSeparator) + "/usr/bin" + string(
		os.PathListSeparator,
	) + "/bin"
	tempDir := filepath.Join(f.tree.root, "tmp")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		t.Fatal(err)
	}

	commandArgs := tofuArgs(workspace, args...)
	command := exec.CommandContext(t.Context(), configsctlBinary, commandArgs...)
	command.Dir = f.tree.root
	command.Env = append(
		[]string{
			"PATH=" + path,
			"HOME=" + f.tree.home,
			"TMPDIR=" + tempDir,
			"FIXTURE_LOCK_DIR=" + f.lockDir,
		},
		f.extraEnv...,
	)
	command.Stdin = strings.NewReader(f.stdin)
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
	f.requireNoAnsibleCall(t)

	return result
}

// requireNoAnsibleCall fails when a sentinel Ansible program ran. The sentinels
// come first on PATH, which keeps the check valid on a machine with Ansible installed.
func (f explicitFixture) requireNoAnsibleCall(t *testing.T) {
	t.Helper()
	marker := filepath.Join(f.binDir, "ansible-marker")
	if body, err := os.ReadFile(marker); err == nil {
		t.Fatalf("configsctl ran an Ansible program: %s", body)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
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

// requireOpenTofuNotStarted reads the command_started log line because a direct
// run leaves no other record that OpenTofu started. The refusal tests also read
// the hostlock.acquired log line because a lock that the command takes and
// releases leaves no file.
func requireOpenTofuNotStarted(t *testing.T, result commandResult) {
	t.Helper()
	if strings.Contains(result.stderr, "command_started") {
		t.Fatalf("OpenTofu started before the refusal: %s", result.stderr)
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
	// The stand-in tofu records "held" when the lock file exists during the apply.
	// The Ansible sentinels in the fixture fail the test through mutate when the
	// command runs an Ansible program.
	state, err := os.ReadFile(filepath.Join(fixture.binDir, "lockstate"))
	if err != nil || strings.TrimSpace(string(state)) != "held" {
		t.Fatalf("the lock file did not exist during the apply: %q, %v", state, err)
	}

	requireNoFile(t, filepath.Join(fixture.lockDir, "deploy.lock"))
	requireFile(t, fixture.stateFile())
	requireNoPlanCopies(t, fixture.tree)
	requireNoExplicitDirectories(t, fixture.tree)
	calls := fixture.calls(t)
	if len(calls) != 3 || !strings.HasPrefix(calls[0], "plan ") ||
		!strings.HasPrefix(calls[1], "show -json ") ||
		!strings.HasPrefix(calls[2], "apply ") {
		t.Fatalf("the controller ran %q, want plan, show, apply", calls)
	}
}

func TestTofuMutationRefusesUndeclaredEndpoint(t *testing.T) {
	missing := strings.Replace(
		explicitSettings,
		"        hv_alpha:\n          address: hv-alpha.example\n",
		"",
		1,
	)
	fixture := newDirectFixture(t, missing)
	result := fixture.apply(t, "-auto-approve", "-input=false")
	if result.exitCode == 0 {
		t.Fatalf("the apply accepted a host without a declared endpoint: %s", result.stderr)
	}

	if !strings.Contains(
		result.stderr,
		"lock_endpoints does not declare host hv_alpha from node_hosts.site",
	) {
		t.Fatalf("the refusal omitted the undeclared host: %s", result.stderr)
	}

	requireOpenTofuNotStarted(t, result)
	if strings.Contains(result.stderr, "hostlock.acquired") {
		t.Fatalf("the refused apply took a lock: %s", result.stderr)
	}

	requireNoFile(t, fixture.stateFile())
}

func TestTofuMutationAppliesTheExactSavedPlan(t *testing.T) {
	t.Run(
		"unchanged plan",
		func(t *testing.T) {
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
			if !strings.HasPrefix(last, "apply -input=false -no-color -parallelism=10 ") ||
				!strings.HasSuffix(last, "/"+explicitPlanFile) {
				t.Fatalf("the apply ran %q, want one saved plan", last)
			}

			requireFile(t, fixture.stateFile())
			requireOnlyRequestVariables(t, filepath.Join(fixture.binDir, "apply-env-names"))
		},
	)
	t.Run(
		"changed plan",
		func(t *testing.T) {
			fixture := newExplicitFixture(t, explicitSettings)
			if err := os.WriteFile(
				filepath.Join(fixture.binDir, "tamper"),
				nil,
				0o600,
			); err != nil {
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
			requireNoFile(t, filepath.Join(fixture.lockDir, "deploy.lock"))
			requireNoExplicitDirectories(t, fixture.tree)
		},
	)
}

// requireOnlyRequestVariables reads the sorted variable names in the environment
// of the apply command. Only the request variables and the variables that the
// shell of the stand-in program adds may appear. The configsctl process
// variables and the lock state variable may not appear.
func requireOnlyRequestVariables(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	allowed := []string{
		"PATH",
		"HOME",
		"TMPDIR",
		"TF_IN_AUTOMATION",
		"TF_VAR_prefixed",
		"LANG",
		"PWD",
		"SHLVL",
		"OLDPWD",
		"_",
	}

	for _, name := range strings.Fields(string(body)) {
		if !slices.Contains(allowed, name) {
			t.Fatalf("the apply command received the variable %s", name)
		}
	}

	if strings.Contains(string(body), hostlock.StateEnvVar) {
		t.Fatalf("the apply command received %s", hostlock.StateEnvVar)
	}
}

func TestTofuMutationRefusesUnknownOwnershipBeforeLocking(t *testing.T) {
	fixture := newDirectFixture(t, explicitSettings)
	unowned := "resource \"terraform_data\" \"unowned\" {\n  input = \"unowned\"\n}\n"
	writeFixtureFile(
		t,
		filepath.Join(fixture.tree.workspaces, "suburban", "tack", "unowned.tf"),
		unowned,
	)
	result := fixture.apply(t, "-auto-approve", "-input=false")
	if result.exitCode == 0 || !strings.Contains(result.stderr, "unknown lock ownership") {
		t.Fatalf("the apply did not refuse unknown ownership: %s", result.stderr)
	}

	if strings.Contains(result.stderr, "hostlock.acquired") {
		t.Fatalf("the refused apply took a lock: %s", result.stderr)
	}

	requireNoFile(t, filepath.Join(fixture.lockDir, "deploy.lock"))
	requireNoFile(t, fixture.stateFile())
}

type explicitOptionCase struct {
	args []string
	want string
}

func TestTofuMutationOptions(t *testing.T) {
	accepted := map[string][]string{
		"-auto-approve":      {"-auto-approve"},
		"-auto-approve=true": {"-auto-approve=true", "-no-color"},
		"-input=false":       {"-auto-approve", "-input=false"},
		"-parallelism=3":     {"-auto-approve", "-parallelism=3"},
		"-lock-timeout=5s":   {"-auto-approve", "-lock-timeout=5s"},
	}
	for name, args := range accepted {
		t.Run(
			"accepts "+name,
			func(t *testing.T) {
				fixture := newExplicitFixture(t, explicitSettings)
				requireSuccess(t, fixture.apply(t, args...), args...)
				calls := fixture.calls(t)
				applied := calls[len(calls)-1]

				for _, option := range args {
					if strings.HasPrefix(option, "-parallelism=") ||
						strings.HasPrefix(option, "-lock-timeout=") {
						if !strings.Contains(applied, option) {
							t.Fatalf("the apply ran %q without %s", applied, option)
						}
					}
				}
			},
		)
	}

	refused := map[string]explicitOptionCase{
		"-target": {
			[]string{"-auto-approve", "-target=module.base"},
			"does not accept the OpenTofu option -target",
		},
		"-var": {
			[]string{"-auto-approve", "-var", "healthy=false"},
			"does not accept the OpenTofu option -var",
		},
		"-var-file": {
			[]string{"-auto-approve", "-var-file=extra.tfvars"},
			"does not accept the OpenTofu option -var-file",
		},
		"-refresh=false": {
			[]string{"-auto-approve", "-refresh=false"},
			"does not accept the OpenTofu option -refresh",
		},
		"-input=true": {[]string{"-auto-approve", "-input=true"}, "requires -input=false"},
	}
	for name, test := range refused {
		t.Run(
			"refuses "+name,
			func(t *testing.T) {
				fixture := newDirectFixture(t, explicitSettings)
				result := fixture.apply(t, test.args...)
				if result.exitCode == 0 || !strings.Contains(result.stderr, test.want) {
					t.Fatalf("the apply did not refuse %s: %s", name, result.stderr)
				}

				requireOpenTofuNotStarted(t, result)
				if strings.Contains(result.stderr, "hostlock.acquired") {
					t.Fatalf("the refused apply took a lock: %s", result.stderr)
				}

				requireNoFile(t, fixture.stateFile())
			},
		)
	}
}

// Real OpenTofu writes the -out file for a plan without changes. The digest
// step needs no special case.
func TestTofuMutationAppliesAgainWithoutChanges(t *testing.T) {
	fixture := newExplicitFixture(t, explicitSettings)
	args := []string{"-auto-approve", "-input=false"}
	requireSuccess(t, fixture.apply(t, args...), args...)
	recorded := filepath.Join(fixture.binDir, "planned.tfplan")
	if err := os.Remove(recorded); err != nil {
		t.Fatal(err)
	}

	requireSuccess(t, fixture.apply(t, args...), args...)
	if info, err := os.Stat(recorded); err != nil || info.Size() == 0 {
		t.Fatalf("OpenTofu did not write the -out file for the plan without changes: %v", err)
	}

	calls := fixture.calls(t)
	if len(calls) != 6 || !strings.HasPrefix(calls[5], "apply ") {
		t.Fatalf("the second run ran %q, want plan, show, apply", calls)
	}

	requireNoExplicitDirectories(t, fixture.tree)
}

const explicitCredentialSettings = `  credentials:
    workspaces/suburban/tack:
      lock_host: hv_alpha
      username_variable: deployment_user
      password_variable: deployment_password
      dell:
        endpoint: https://127.0.0.1:1
        rotator_user: rotator
        rotator_password_key: vault_rotator
        account_id: "3"
        username: configsctl
        read_paths:
          - /redfish/v1/Managers/iDRAC.Embedded.1
`

type legacyRefusalCase struct {
	settings string
	args     []string
	savePlan bool
	want     string
}

func TestTofuMutationRefusesTheInventoryLockPath(t *testing.T) {
	withoutGuestModules := strings.Replace(
		explicitSettings,
		"  guest_modules:\n    suburban/tack: module.base\n",
		"",
		1,
	)

	cases := map[string]legacyRefusalCase{
		"guest modules unset": {
			withoutGuestModules,
			[]string{"apply", "-auto-approve", "-input=false"},
			false,
			"tofu.guest_modules is unset",
		},
		"saved plan": {
			explicitSettings,
			[]string{"apply", "-input=false", "saved.plan"},
			true,
			"the command includes a saved plan",
		},
		"option before apply": {
			explicitSettings,
			[]string{"-input=false", "apply", "-auto-approve"},
			false,
			"the command must start with apply",
		},
		"destroy": {
			explicitSettings,
			[]string{"destroy", "-auto-approve", "-input=false"},
			false,
			"runs apply only",
		},
	}
	for name, test := range cases {
		t.Run(
			name,
			func(t *testing.T) {
				fixture := newDirectFixture(t, test.settings)
				if test.savePlan {
					plan := tofuArgs(explicitWorkspace, "plan", "-input=false", "-out=saved.plan")
					requireSuccess(t, runConfigsctl(t, fixture.tree, plan...), plan...)
				}

				result := fixture.mutate(t, test.args...)
				if result.exitCode == 0 {
					t.Fatalf("the command ran on the inventory lock path: %s", result.stderr)
				}

				for _, fragment := range []string{
					"workspaces/suburban/tack",
					"lock_endpoints [guest-alpha hv_alpha]",
					"the explicit lock path cannot run",
					test.want,
				} {
					if !strings.Contains(result.stderr, fragment) {
						t.Fatalf("the refusal omitted %q: %s", fragment, result.stderr)
					}
				}

				requireOpenTofuNotStarted(t, result)
				if strings.Contains(result.stderr, "hostlock.acquired") {
					t.Fatalf("the refused command took a lock: %s", result.stderr)
				}

				requireNoFile(t, fixture.stateFile())
			},
		)
	}
}

func TestEndpointCredentialRequiresEphemeralInputsBeforeAuthentication(t *testing.T) {
	fixture := newDirectFixture(t, explicitSettings+explicitCredentialSettings)
	result := fixture.mutate(t, "plan", "-input=false")
	if result.exitCode == 0 ||
		!strings.Contains(result.stderr, "credential variable deployment_user does not exist") {
		t.Fatalf("the endpoint credential accepted missing ephemeral inputs: %s", result.stderr)
	}

	if strings.Contains(result.stderr, "hostlock.acquired") {
		t.Fatal("the invalid endpoint credential acquired a host lock")
	}

	requireOpenTofuNotStarted(t, result)
}

func TestHelpRunsWithoutLocksInEndpointWorkspaces(t *testing.T) {
	withoutGuestModules := strings.Replace(
		explicitSettings,
		"  guest_modules:\n    suburban/tack: module.base\n",
		"",
		1,
	)

	cases := map[string]struct {
		settings string
		args     []string
	}{
		"apply with guest modules unset":      {withoutGuestModules, []string{"apply", "-help"}},
		"destroy with guest modules unset":    {withoutGuestModules, []string{"destroy", "-help"}},
		"apply -h with guest modules unset":   {withoutGuestModules, []string{"apply", "-h"}},
		"destroy -h with guest modules unset": {withoutGuestModules, []string{"destroy", "-h"}},
		"apply with a saved plan": {
			explicitSettings,
			[]string{"apply", "-help", "-input=false", "saved.plan"},
		},
	}
	for name, test := range cases {
		t.Run(
			name,
			func(t *testing.T) {
				fixture := newDirectFixture(t, test.settings)
				result := fixture.mutate(t, test.args...)
				// The exit status and the usage text belong to OpenTofu and differ between
				// versions. The test checks only what configsctl did.
				if strings.Count(result.stderr, "command_started") > 1 ||
					strings.Contains(result.stderr, "tofurun.command_started") {
					t.Fatalf(
						"configsctl ran more than the OpenTofu help command: %s",
						result.stderr,
					)
				}

				// No program or file records a lock that the command takes and releases, so
				// the test reads the hostlock log line. The Ansible sentinels cover the
				// inventory and Ansible use.
				if strings.Contains(result.stderr, "hostlock.acquired") {
					t.Fatalf("the help command took a lock: %s", result.stderr)
				}

				requireNoFile(t, filepath.Join(fixture.lockDir, "deploy.lock"))
			},
		)
	}
}

func TestTofuMutationConfirmation(t *testing.T) {
	refusals := map[string]struct {
		args     []string
		extraEnv []string
	}{
		"input option":  {[]string{"-input=false"}, nil},
		"TF_INPUT is 0": {nil, []string{"TF_INPUT=0"}},
	}
	for name, test := range refusals {
		t.Run(
			"refuses "+name,
			func(t *testing.T) {
				fixture := newDirectFixture(t, explicitSettings)
				fixture.extraEnv = test.extraEnv
				result := fixture.apply(t, test.args...)
				if result.exitCode == 0 ||
					!strings.Contains(
						result.stderr,
						"OpenTofu requires -auto-approve for a noninteractive apply",
					) {
					t.Fatalf("the apply did not require -auto-approve: %s", result.stderr)
				}

				if strings.Contains(result.stderr, "hostlock.acquired") {
					t.Fatalf("the refused apply took a lock: %s", result.stderr)
				}

				requireNoFile(t, fixture.stateFile())
			},
		)
	}

	t.Run(
		"applies after yes",
		func(t *testing.T) {
			fixture := newDirectFixture(t, explicitSettings)
			fixture.stdin = "yes\n"
			requireSuccess(t, fixture.apply(t), "apply")
			requireFile(t, fixture.stateFile())
			requireNoFile(t, filepath.Join(fixture.lockDir, "deploy.lock"))
		},
	)
	t.Run(
		"stops after another answer",
		func(t *testing.T) {
			fixture := newDirectFixture(t, explicitSettings)
			fixture.stdin = "cancel\n"
			result := fixture.apply(t)
			if result.exitCode == 0 || !strings.Contains(result.stderr, "apply canceled") {
				t.Fatalf("the apply ignored the canceled confirmation: %s", result.stderr)
			}

			if strings.Contains(result.stderr, "hostlock.acquired") {
				t.Fatalf("the canceled apply took a lock: %s", result.stderr)
			}

			requireNoFile(t, fixture.stateFile())
		},
	)
}

func TestTofuMutationChecksTheWorkspaceOverride(t *testing.T) {
	t.Run(
		"refuses a different workspace",
		func(t *testing.T) {
			fixture := newDirectFixture(t, explicitSettings)
			fixture.extraEnv = []string{"TF_WORKSPACE=prod"}
			result := fixture.apply(t, "-auto-approve", "-input=false")
			if result.exitCode == 0 || !strings.Contains(result.stderr, "requested=prod") ||
				!strings.Contains(result.stderr, "selected=default") {
				t.Fatalf("the apply did not refuse the workspace override: %s", result.stderr)
			}

			requireOpenTofuNotStarted(t, result)
			if strings.Contains(result.stderr, "hostlock.acquired") {
				t.Fatalf("the refused apply took a lock: %s", result.stderr)
			}

			requireNoFile(t, fixture.stateFile())
		},
	)
	t.Run(
		"accepts the selected workspace",
		func(t *testing.T) {
			fixture := newDirectFixture(t, explicitSettings)
			fixture.extraEnv = []string{"TF_WORKSPACE=default"}
			args := []string{"-auto-approve", "-input=false"}
			requireSuccess(t, fixture.apply(t, args...), args...)
			requireFile(t, fixture.stateFile())
		},
	)
}
