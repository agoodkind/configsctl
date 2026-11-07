//go:build unix

package main_test

import (
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
	if !strings.Contains(plan.stderr, dockerBridgeStarted+" aliases=[guest_alpha] get_only=true") {
		t.Fatalf("the plan did not start a read-only Docker bridge: %s", plan.stderr)
	}
	fixture.requireNoDockerBridgeDirectories(t)

	apply := fixture.apply(t, "-auto-approve", "-input=false", variableFile)
	requireSuccess(t, apply)
	if !strings.Contains(apply.stderr, dockerBridgeStarted+" aliases=[guest_alpha] get_only=false") {
		t.Fatalf("the apply did not start a writable Docker bridge: %s", apply.stderr)
	}
	fixture.requireBridgeSocketUsed(t, plan, apply)
}

func TestTofuDockerBridgeServesSavedPlanApply(t *testing.T) {
	fixture, variableFile := newDecoyDockerBridgeFixture(t)
	plan := fixture.mutate(t, "plan", "-input=false", variableFile, "-out=bridge.plan")
	requireSuccess(t, plan)
	apply := fixture.mutate(t, "apply", "-input=false", variableFile, "bridge.plan")
	requireSuccess(t, apply)
	fixture.requireBridgeSocketUsed(t, plan, apply)
}

type dockerBridgeRefusal struct {
	configuration string
	workspace     string
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
			workspace:     workspace, extraEnv: nil, args: nil,
			want: "docker_bridges.guest_alpha uses guest site/202, which guest_hosts does not declare",
		},
		"docker_hosts not ephemeral": {
			configuration: configuration,
			workspace:     strings.Replace(workspace, "  type      = map(string)\n  ephemeral = true\n", "  type = map(string)\n", 1),
			extraEnv:      nil, args: nil,
			want: "must declare the ephemeral variable docker_hosts",
		},
		"token variable not ephemeral": {
			configuration: configuration,
			workspace:     strings.Replace(workspace, "  sensitive = true\n  ephemeral = true\n", "  sensitive = true\n", 1),
			extraEnv:      nil, args: nil,
			want: "must declare the variable vault_alpha with ephemeral = true for the token of the Docker bridge guest_alpha",
		},
		"token variable undeclared": {
			configuration: strings.Replace(configuration, dockerBridgeToken, `token_variable  = "vault_missing"`, 1),
			workspace:     workspace, extraEnv: nil, args: nil,
			want: "must declare the variable vault_missing with ephemeral = true for the token of the Docker bridge guest_alpha",
		},
		"missing token secret": {
			configuration: strings.Replace(configuration, dockerBridgeToken, `token_variable  = "vault_unset"`, 1),
			workspace:     strings.Replace(workspace, `variable "vault_alpha"`, `variable "vault_unset"`, 1),
			extraEnv:      nil, args: nil,
			want: "the Docker bridge guest_alpha requires the token secret TF_VAR_vault_unset; add the vault key vault_unset",
		},
		"operator docker_hosts": {
			configuration: configuration, workspace: workspace,
			extraEnv: []string{"TF_VAR_docker_hosts=" + operatorHosts}, args: nil,
			want: "TF_VAR_docker_hosts conflicts with the Docker bridges for [guest_alpha]",
		},
		"operator docker_hosts argument": {
			configuration: configuration, workspace: workspace,
			extraEnv: nil, args: []string{"-var", "docker_hosts=" + operatorHosts},
			want: "-var docker_hosts conflicts with the Docker bridges for [guest_alpha]",
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newDockerBridgeFixture(t, test.configuration, test.workspace)
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
