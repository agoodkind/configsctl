//go:build unix

package main_test

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	dockerBridgeStarted   = "docker_bridge.started"
	dockerBridgeDirectory = "configsctl-docker-*"
	dockerBridgeResult    = "bridge-socket.txt"
	dockerBridgeVMID      = "vmid            = local.guest.vmid"
	dockerBridgeToken     = `token_variable  = "vault_alpha"`
	operatorTokenBytes    = 16
)

func dockerBridgeFixtureFile(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "dockerbridge", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

type dockerBridgeFixture struct {
	explicitFixture
	temporary string
}

// newDockerBridgeFixture installs a native workspace and sets a short TMPDIR
// to keep socket paths within the macOS socket path limit.
func newDockerBridgeFixture(t *testing.T, configuration, workspace string) dockerBridgeFixture {
	t.Helper()
	temporary, err := os.MkdirTemp(shortTempRoot, "cdb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(temporary) })
	fixture := dockerBridgeFixture{explicitFixture: newDirectFixture(t, explicitSettings), temporary: temporary}
	fixture.extraEnv = []string{"TMPDIR=" + temporary}
	installNativeConfiguration(t, fixture.tree)
	writeFixtureFile(t, filepath.Join(fixture.tree.root, "native", "suburban", "tack", "main.tf"), configuration)
	root := filepath.Join(fixture.tree.workspaces, "suburban", "tack")
	writeFixtureFile(t, filepath.Join(root, "main.tf"), workspace)
	writeFixtureFile(t, filepath.Join(root, "item", "main.tf"), dockerBridgeFixtureFile(t, "item.tf"))
	writeFixtureFile(t, filepath.Join(root, "item", "bridge-socket.sh"), dockerBridgeFixtureFile(t, "bridge-socket.sh"))
	return fixture
}

func (f dockerBridgeFixture) requireNoDockerBridgeDirectories(t *testing.T) {
	t.Helper()
	for _, pattern := range []string{dockerBridgeDirectory, "configsctl-explicit-*"} {
		leftovers, err := filepath.Glob(filepath.Join(f.temporary, pattern))
		if err != nil {
			t.Fatal(err)
		}
		if len(leftovers) != 0 {
			t.Fatalf("TMPDIR has Docker bridge directories after configsctl exited: %v", leftovers)
		}
	}
}

// newDecoyDockerBridgeFixture adds an automatic variable file and returns a
// -var-file option. Both assign docker_hosts the same decoy socket path.
func newDecoyDockerBridgeFixture(t *testing.T) (dockerBridgeFixture, string) {
	t.Helper()
	fixture := newDockerBridgeFixture(t, dockerBridgeFixtureFile(t, "configuration.tf"), dockerBridgeFixtureFile(t, "root.tf"))
	decoy := dockerBridgeFixtureFile(t, "decoy.tfvars")
	writeFixtureFile(t, filepath.Join(fixture.tree.workspaces, "suburban", "tack", "decoy.auto.tfvars"), decoy)
	variables := filepath.Join(t.TempDir(), "decoy.tfvars")
	writeFixtureFile(t, variables, decoy)
	return fixture, "-var-file=" + variables
}

// requireBridgeSocketUsed checks that the socket path the apply provisioner
// found starts with the bridge TMPDIR and does not exist after the command.
func (f dockerBridgeFixture) requireBridgeSocketUsed(t *testing.T, results ...commandResult) {
	t.Helper()
	recorded, err := os.ReadFile(filepath.Join(f.tree.workspaces, "suburban", "tack", "item", dockerBridgeResult))
	if err != nil {
		t.Fatalf("the apply provisioner did not find the Docker bridge socket: %v", err)
	}
	socket := strings.TrimSpace(string(recorded))
	if !strings.HasPrefix(socket, f.temporary+string(filepath.Separator)) {
		t.Fatalf("the apply used the socket %s instead of a Docker bridge socket in %s", socket, f.temporary)
	}
	requireNoFile(t, socket)
	f.requireNoDockerBridgeDirectories(t)
	state, err := os.ReadFile(f.stateFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(state), f.temporary) {
		t.Fatal("the OpenTofu state stored the Docker bridge socket map")
	}
	for _, result := range results {
		if strings.Contains(result.stdout+result.stderr, vaultAlphaValue) {
			t.Fatal("the command output disclosed the Docker bridge token secret")
		}
	}
}

func TestTofuDockerBridgeServesNativePlanAndApply(t *testing.T) {
	fixture, variableFile := newDecoyDockerBridgeFixture(t)

	plan := fixture.mutate(t, "plan", "-input=false", variableFile)
	requireSuccess(t, plan)
	if !strings.Contains(plan.stderr, dockerBridgeStarted+" aliases=[guest_alpha] writable=[]") {
		t.Fatalf("the plan did not start a read-only Docker bridge: %s", plan.stderr)
	}
	fixture.requireNoDockerBridgeDirectories(t)

	apply := fixture.apply(t, "-auto-approve", "-input=false", variableFile)
	requireSuccess(t, apply)
	if !strings.Contains(apply.stderr, dockerBridgeStarted+" aliases=[guest_alpha] writable=[guest_alpha]") {
		t.Fatalf("the apply did not start a writable Docker bridge: %s", apply.stderr)
	}
	fixture.requireBridgeSocketUsed(t, plan, apply)
}

func TestTofuDockerBridgeServesSavedPlanApply(t *testing.T) {
	fixture, variableFile := newDecoyDockerBridgeFixture(t)
	plan := fixture.mutate(t, "plan", "-input=false", variableFile, "-out=bridge.plan")
	requireSuccess(t, plan)
	apply := fixture.mutate(t, "apply", "-input=false", "bridge.plan", variableFile)
	requireSuccess(t, apply)
	fixture.requireBridgeSocketUsed(t, plan, apply)
}

type dockerBridgeRefusal struct {
	configuration string
	workspace     string
	files         map[string]string
	extraEnv      []string
	args          []string
	want          string
}

func TestTofuRefusesInvalidDockerBridges(t *testing.T) {
	configuration := dockerBridgeFixtureFile(t, "configuration.tf")
	workspace := dockerBridgeFixtureFile(t, "root.tf")
	operatorHosts := `{"guest_alpha":"unix:///tmp/operator.sock"}`
	cases := map[string]dockerBridgeRefusal{
		"guest outside guest_hosts": {
			configuration: strings.Replace(configuration, dockerBridgeVMID, "vmid            = 202", 1),
			workspace:     workspace, files: nil, extraEnv: nil, args: nil,
			want: "docker_bridges.guest_alpha uses guest site/202, which guest_hosts does not declare",
		},
		"docker_hosts not ephemeral": {
			configuration: configuration,
			workspace:     strings.Replace(workspace, "  type      = map(string)\n  ephemeral = true\n", "  type = map(string)\n", 1),
			files:         nil, extraEnv: nil, args: nil,
			want: "must declare the ephemeral variable docker_hosts",
		},
		"token variable not ephemeral": {
			configuration: configuration,
			workspace:     strings.Replace(workspace, "  sensitive = true\n  ephemeral = true\n", "  sensitive = true\n", 1),
			files:         nil, extraEnv: nil, args: nil,
			want: "must declare the variable vault_alpha with ephemeral = true for the token of the Docker bridge guest_alpha",
		},
		"token variable undeclared": {
			configuration: strings.Replace(configuration, dockerBridgeToken, `token_variable  = "vault_missing"`, 1),
			workspace:     workspace, files: nil, extraEnv: nil, args: nil,
			want: "must declare the variable vault_missing with ephemeral = true for the token of the Docker bridge guest_alpha",
		},
		"missing token secret": {
			configuration: strings.Replace(configuration, dockerBridgeToken, `token_variable  = "vault_unset"`, 1),
			workspace:     strings.Replace(workspace, `variable "vault_alpha"`, `variable "vault_unset"`, 1),
			files:         nil, extraEnv: nil, args: nil,
			want: "the Docker bridge guest_alpha requires the token secret TF_VAR_vault_unset; add the vault key vault_unset",
		},
		"operator docker_hosts": {
			configuration: configuration, workspace: workspace,
			files: nil, extraEnv: []string{"TF_VAR_docker_hosts=" + operatorHosts}, args: nil,
			want: "TF_VAR_docker_hosts conflicts with the Docker bridges for [guest_alpha]",
		},
		"operator docker_hosts argument": {
			configuration: configuration, workspace: workspace,
			files: nil, extraEnv: nil, args: []string{"-var", "docker_hosts=" + operatorHosts},
			want: "-var docker_hosts conflicts with the Docker bridges for [guest_alpha]",
		},
		"docker_hosts override sets ephemeral false": {
			configuration: configuration, workspace: workspace,
			files:    map[string]string{"a_override.tf": dockerBridgeFixtureFile(t, "hosts-false-override.tf")},
			extraEnv: nil, args: nil,
			want: "must declare the ephemeral variable docker_hosts",
		},
		"chdir redirection": {
			configuration: configuration, workspace: workspace,
			files: nil, extraEnv: nil, args: []string{"-chdir=" + t.TempDir()},
			want: "-chdir selects another configuration",
		},
		"config redirection": {
			configuration: configuration, workspace: workspace,
			files: nil, extraEnv: nil, args: []string{"-config=" + t.TempDir()},
			want: "-config selects another configuration",
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newDockerBridgeFixture(t, test.configuration, test.workspace)
			for file, body := range test.files {
				writeFixtureFile(t, filepath.Join(fixture.tree.workspaces, "suburban", "tack", file), body)
			}
			fixture.extraEnv = append(fixture.extraEnv, test.extraEnv...)
			result := fixture.mutate(t, append([]string{"plan", "-input=false"}, test.args...)...)
			if result.exitCode == 0 || !strings.Contains(result.stderr, test.want) {
				t.Fatalf("the plan did not refuse the Docker bridge declaration with %q: %s", test.want, result.stderr)
			}
			if strings.Contains(result.stderr, dockerBridgeStarted) || strings.Contains(result.stderr, "hostlock.acquired") {
				t.Fatalf("the refused plan started a Docker bridge or took a lock: %s", result.stderr)
			}
			fixture.requireNoDockerBridgeDirectories(t)
		})
	}
}

func TestTofuDockerBridgeKeepsFlagsAfterDescriptionOverride(t *testing.T) {
	fixture := newDockerBridgeFixture(t, dockerBridgeFixtureFile(t, "configuration.tf"), dockerBridgeFixtureFile(t, "root.tf"))
	override := filepath.Join(fixture.tree.workspaces, "suburban", "tack", "override.tf")
	writeFixtureFile(t, override, dockerBridgeFixtureFile(t, "hosts-description-override.tf"))
	plan := fixture.mutate(t, "plan", "-input=false")
	requireSuccess(t, plan)
	if !strings.Contains(plan.stderr, dockerBridgeStarted+" aliases=[guest_alpha] writable=[]") {
		t.Fatalf("the description-only override stopped the Docker bridge plan: %s", plan.stderr)
	}
}

// TestTofuDockerBridgeOverridesLaterImportVariables places the decoy
// -var-file after the import address and ID. The docker_hosts validation rule
// fails when the decoy value wins.
func TestTofuDockerBridgeOverridesLaterImportVariables(t *testing.T) {
	fixture, variableFile := newDecoyDockerBridgeFixture(t)
	address := `module.base["guest_alpha"].terraform_data.item`
	result := fixture.mutate(t, "import", "-input=false", address, "00000000-0000-4000-8000-000000000001", variableFile)
	requireSuccess(t, result)
	if !strings.Contains(result.stderr, dockerBridgeStarted+" aliases=[guest_alpha] writable=[]") {
		t.Fatalf("the import did not start a read-only Docker bridge: %s", result.stderr)
	}
	state := fixture.mutate(t, "state", "list")
	requireSuccess(t, state)
	if !strings.Contains(state.stdout, address) {
		t.Fatalf("the import did not add %s to the state: %s", address, state.stdout)
	}
	fixture.requireNoDockerBridgeDirectories(t)
}

// TestTofuDockerBridgeWritesOnlyLockedGuests applies a change that locks only
// guest-alpha. The provisioner sends POST /volumes/create through both
// sockets. A read-only proxy answers 501 before it contacts Proxmox, and the
// writable proxy answers 502 because the Proxmox endpoint refuses connections.
func TestTofuDockerBridgeWritesOnlyLockedGuests(t *testing.T) {
	fixture := newDockerBridgeFixture(t, dockerBridgeFixtureFile(t, "configuration-two.tf"), dockerBridgeFixtureFile(t, "root.tf"))
	item := filepath.Join(fixture.tree.workspaces, "suburban", "tack", "item")
	writeFixtureFile(t, filepath.Join(item, "main.tf"), dockerBridgeFixtureFile(t, "item-probe.tf"))
	writeFixtureFile(t, filepath.Join(item, "bridge-probe.sh"), dockerBridgeFixtureFile(t, "bridge-probe.sh"))
	apply := fixture.apply(t, "-auto-approve", "-input=false")
	requireSuccess(t, apply)
	if !strings.Contains(apply.stderr, dockerBridgeStarted+" aliases=\"[guest_alpha guest_beta]\" writable=[guest_alpha]") {
		t.Fatalf("the apply did not limit writes to the locked guest: %s", apply.stderr)
	}
	probe, err := os.ReadFile(filepath.Join(item, "bridge-probe.txt"))
	if err != nil {
		t.Fatalf("the apply provisioner did not probe the Docker bridges: %v", err)
	}
	if string(probe) != "guest_alpha=502\nguest_beta=501\n" {
		t.Fatalf("the Docker bridges answered %q, want a writable guest_alpha and a read-only guest_beta", probe)
	}
	fixture.requireNoDockerBridgeDirectories(t)
}

func TestTofuNativeOutputsRequireExactNames(t *testing.T) {
	t.Run("LOCK_TARGETS", func(t *testing.T) {
		fixture := newDockerBridgeFixture(t, dockerBridgeFixtureFile(t, "configuration.tf"), dockerBridgeFixtureFile(t, "root.tf"))
		writeFixtureFile(t, filepath.Join(fixture.tree.root, "native", "main.tf"), dockerBridgeFixtureFile(t, "native-root-upper-lock.tf"))
		result := fixture.mutate(t, "plan", "-input=false")
		if result.exitCode == 0 || !strings.Contains(result.stderr, "must export lock_targets") {
			t.Fatalf("the plan accepted LOCK_TARGETS as lock_targets: %s", result.stderr)
		}
	})
	t.Run("DOCKER_BRIDGES", func(t *testing.T) {
		fixture := newDirectFixture(t, explicitSettings)
		installNativeConfiguration(t, fixture.tree)
		writeFixtureFile(t, filepath.Join(fixture.tree.root, "native", "suburban", "tack", "main.tf"), dockerBridgeFixtureFile(t, "configuration.tf"))
		writeFixtureFile(t, filepath.Join(fixture.tree.root, "native", "main.tf"), dockerBridgeFixtureFile(t, "native-root-upper-bridges.tf"))
		result := fixture.mutate(t, "plan", "-input=false")
		requireSuccess(t, result)
		if strings.Contains(result.stderr, dockerBridgeStarted) {
			t.Fatalf("the plan used DOCKER_BRIDGES as docker_bridges: %s", result.stderr)
		}
	})
}

func syntheticOperatorToken(t *testing.T, prefix string) string {
	t.Helper()
	random := make([]byte, operatorTokenBytes)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(random)
}

func TestTofuDockerBridgeRedactsOperatorToken(t *testing.T) {
	environmentToken := syntheticOperatorToken(t, "synthetic-environment-token-")
	argumentToken := syntheticOperatorToken(t, "synthetic-argument-token-")
	configuration := strings.Replace(dockerBridgeFixtureFile(t, "configuration.tf"), dockerBridgeToken, `token_variable  = "vault_operator"`, 1)
	workspace := strings.Replace(dockerBridgeFixtureFile(t, "root.tf"), `variable "vault_alpha"`, `variable "vault_operator"`, 1)
	address := `module.base["guest_alpha"].terraform_data.item`
	commands := map[string][]string{
		"plan":   {"plan", "-input=false", "-var=vault_operator=" + argumentToken},
		"import": {"import", "-input=false", address, "00000000-0000-4000-8000-000000000002", "-var", "vault_operator=" + argumentToken},
	}
	for name, args := range commands {
		t.Run(name, func(t *testing.T) {
			fixture := newDockerBridgeFixture(t, configuration, workspace)
			fixture.extraEnv = append(fixture.extraEnv, "TF_VAR_vault_operator="+environmentToken)
			result := fixture.mutate(t, args...)
			requireSuccess(t, result)
			if !strings.Contains(result.stderr, dockerBridgeStarted) {
				t.Fatalf("the %s did not start the Docker bridge: %s", name, result.stderr)
			}
			output := result.stdout + result.stderr
			if strings.Contains(output, environmentToken) || strings.Contains(output, argumentToken) {
				t.Fatalf("the %s output disclosed an operator Docker bridge token", name)
			}
		})
	}
}
