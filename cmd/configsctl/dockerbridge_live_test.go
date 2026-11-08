//go:build unix

package main_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"text/template"
	"time"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	liveSelectorVariable    = "TF_ACC"
	liveEndpointVariable    = "PVEGUEST_ACC_ENDPOINT"
	liveNodeVariable        = "PVEGUEST_ACC_NODE"
	liveTokenFileVariable   = "PVEGUEST_ACC_TOKEN_FILE"
	liveKindVariable        = "PVEGUEST_ACC_KIND"
	liveInsecureVariable    = "PVEGUEST_ACC_INSECURE"
	liveVMIDVariable        = "CONFIGSCTL_DOCKER_BRIDGE_VMID"
	liveLockAddressVariable = "CONFIGSCTL_DOCKER_BRIDGE_LOCK_ADDRESS"
	liveLockUserVariable    = "CONFIGSCTL_DOCKER_BRIDGE_LOCK_USER"
	liveAgentVariable       = "SSH_AUTH_SOCK"
	liveTokenVariable       = "vault_docker_bridge_token"
	liveLockHost            = "docker-bridge-guest"
	liveLockFile            = "/var/lib/configs/deploy.lock"
	liveRunPrefix           = "configsctl-bridge-acc-"
	liveRunBytes            = 6
	liveGuestSeconds        = 60
	livePauseWait           = 10 * time.Minute
	liveExitWait            = 2 * time.Minute
	livePollInterval        = time.Second
	liveFirstKey            = "first"
	liveSecondKey           = "second"
	liveFirstAddress        = `module.base["guest"].docker_volume.test["first"]`
)

type liveBridgeInputs struct {
	Endpoint       string
	Node           string
	Kind           string
	TokenPrincipal string
	TokenVariable  string
	LockHost       string
	LockUser       string
	LockAddress    string
	VMID           int64
	Insecure       bool
	token          string
	secret         string
}

type liveRoot struct {
	TokenVariable string
	RunLabel      string
	PauseMarker   string
	Volumes       map[string]string
}

type liveGuest struct {
	client *transport.Client
	guest  transport.Guest
}

func requireLiveVariable(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s must be set when %s selects the live Docker bridge test", name, liveSelectorVariable)
	}
	return value
}

func requireLiveInputs(t *testing.T) liveBridgeInputs {
	t.Helper()
	if os.Getenv(liveSelectorVariable) == "" {
		t.Skipf("%s does not select live tests", liveSelectorVariable)
	}
	tokenBytes, err := os.ReadFile(requireLiveVariable(t, liveTokenFileVariable))
	if err != nil {
		t.Fatalf("read %s: %v", liveTokenFileVariable, err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	principal, secret, found := strings.Cut(token, "=")
	if !found || principal == "" || secret == "" {
		t.Fatalf("%s must contain user@realm!tokenid=secret", liveTokenFileVariable)
	}
	vmid, err := strconv.ParseInt(requireLiveVariable(t, liveVMIDVariable), 10, 64)
	if err != nil {
		t.Fatalf("%s: %v", liveVMIDVariable, err)
	}
	insecure := false
	if text := os.Getenv(liveInsecureVariable); text != "" {
		insecure, err = strconv.ParseBool(text)
		if err != nil {
			t.Fatalf("%s: %v", liveInsecureVariable, err)
		}
	}
	return liveBridgeInputs{
		Endpoint: requireLiveVariable(t, liveEndpointVariable), Node: requireLiveVariable(t, liveNodeVariable),
		Kind: requireLiveVariable(t, liveKindVariable), TokenPrincipal: principal, TokenVariable: liveTokenVariable,
		LockHost: liveLockHost, LockUser: requireLiveVariable(t, liveLockUserVariable),
		LockAddress: requireLiveVariable(t, liveLockAddressVariable), VMID: vmid, Insecure: insecure,
		token: token, secret: secret,
	}
}

func renderLiveTemplate(t *testing.T, name string, data any) string {
	t.Helper()
	parsed, err := template.New(name).Option("missingkey=error").Parse(dockerBridgeFixtureFile(t, name))
	if err != nil {
		t.Fatal(err)
	}
	var rendered bytes.Buffer
	if err := parsed.Execute(&rendered, data); err != nil {
		t.Fatal(err)
	}
	return rendered.String()
}

func newLiveGuest(t *testing.T, inputs liveBridgeInputs) liveGuest {
	t.Helper()
	client, err := transport.NewClient(map[string]transport.NodeConfig{inputs.Node: {
		Endpoint: inputs.Endpoint, APIToken: inputs.token, Insecure: inputs.Insecure, NodeName: inputs.Node, MaxExecRequests: 0,
	}}, transport.DefaultMaxRequests)
	if err != nil {
		t.Fatal(err)
	}
	return liveGuest{client: client, guest: transport.Guest{Node: inputs.Node, VMID: inputs.VMID, Kind: transport.Kind(inputs.Kind)}}
}

func (g liveGuest) succeeds(t *testing.T, argv ...string) bool {
	t.Helper()
	result, err := g.client.RunReadOnly(t.Context(), g.guest, transport.Command{Argv: argv, Stdin: nil, TimeoutSeconds: liveGuestSeconds})
	if err != nil {
		t.Fatalf("run %q in the guest: %v", argv, err)
	}
	return result.ExitCode == 0
}

func (g liveGuest) volumeExists(t *testing.T, name string) bool {
	t.Helper()
	return g.succeeds(t, "docker", "volume", "inspect", "--format", "{{.Name}}", name)
}

func (g liveGuest) removeVolume(t *testing.T, name string) {
	t.Helper()
	result, err := g.client.Run(t.Context(), g.guest, transport.Command{Argv: []string{"docker", "volume", "rm", name}, Stdin: nil, TimeoutSeconds: liveGuestSeconds})
	if err != nil || result.ExitCode != 0 {
		t.Errorf("remove the test volume %s: %v %s", name, err, result.Stderr)
	}
}

func newLiveBridgeFixture(t *testing.T, inputs liveBridgeInputs) dockerBridgeFixture {
	t.Helper()
	temporary, err := os.MkdirTemp(shortTempRoot, "cdb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(temporary) })
	tree := newConfigsTree(t)
	installNativeConfiguration(t, tree)
	writeFixtureFile(t, filepath.Join(tree.root, "native", "suburban", "tack", "main.tf"), renderLiveTemplate(t, "live-configuration.tf.tmpl", inputs))
	root := filepath.Join(tree.workspaces, "suburban", "tack")
	writeFixtureFile(t, filepath.Join(root, "backend.tf"), fmt.Sprintf(localBackend, "tack.tfstate"))
	writeFixtureFile(t, filepath.Join(root, "item", "main.tf"), dockerBridgeFixtureFile(t, "live-item.tf"))
	writeFixtureFile(t, filepath.Join(root, "item", "live-pause.sh"), dockerBridgeFixtureFile(t, "live-pause.sh"))
	binDir := t.TempDir()
	installLiveStandIns(t, binDir)
	environment := []string{"TMPDIR=" + temporary, "TF_VAR_" + inputs.TokenVariable + "=" + inputs.secret}
	if agent := os.Getenv(liveAgentVariable); agent != "" {
		environment = append(environment, liveAgentVariable+"="+agent)
	}
	return dockerBridgeFixture{
		explicitFixture: explicitFixture{tree: tree, binDir: binDir, lockDir: t.TempDir(), extraEnv: environment, stdin: "", observed: false},
		temporary:       temporary,
	}
}

func installLiveStandIns(t *testing.T, binDir string) {
	t.Helper()
	realTofu, err := exec.LookPath("tofu")
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(realTofu)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(absolute, filepath.Join(binDir, "tofu")); err != nil {
		t.Fatal(err)
	}
	sentinel, err := os.ReadFile(filepath.Join("testdata", "explicit", "ansible"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range ansibleProgramNames {
		if err := os.WriteFile(filepath.Join(binDir, name), sentinel, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func (f dockerBridgeFixture) writeLiveRoot(t *testing.T, root liveRoot) {
	t.Helper()
	writeFixtureFile(t, filepath.Join(f.tree.workspaces, "suburban", "tack", "main.tf"), renderLiveTemplate(t, "live-root.tf.tmpl", root))
}

func (f dockerBridgeFixture) startLive(t *testing.T, args ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	publishConfigsTree(t, f.tree)
	path := f.binDir + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin"
	command := exec.CommandContext(t.Context(), configsctlBinary, tofuArgs(explicitWorkspace, args...)...)
	command.Dir = f.tree.root
	command.Env = append([]string{"PATH=" + path, "HOME=" + f.tree.home, "FIXTURE_LOCK_DIR=" + f.lockDir}, f.extraEnv...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	return command, &stdout, &stderr
}

func (f dockerBridgeFixture) liveSockets(t *testing.T) []string {
	t.Helper()
	sockets, err := filepath.Glob(filepath.Join(f.temporary, "configsctl-explicit-*", "docker", "*.sock"))
	if err != nil {
		t.Fatal(err)
	}
	return sockets
}

func waitForMarker(t *testing.T, marker string, exited <-chan error) {
	t.Helper()
	deadline := time.After(livePauseWait)
	ticker := time.NewTicker(livePollInterval)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		select {
		case err := <-exited:
			t.Fatalf("configsctl exited before the pause provisioner started: %v", err)
		case <-deadline:
			t.Fatalf("the pause provisioner did not start within %s", livePauseWait)
		case <-ticker.C:
		}
	}
}

func requireNoLiveSecret(t *testing.T, inputs liveBridgeInputs, texts ...string) {
	t.Helper()
	for _, text := range texts {
		if strings.Contains(text, inputs.secret) {
			t.Fatal("the live Docker bridge output or state disclosed the token secret")
		}
	}
}

func TestTofuDockerBridgeLifecycle(t *testing.T) {
	inputs := requireLiveInputs(t)
	requireTofu(t)
	guest := newLiveGuest(t, inputs)
	random := make([]byte, liveRunBytes)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	runLabel := liveRunPrefix + hex.EncodeToString(random)
	volumes := map[string]string{liveFirstKey: runLabel + "-" + liveFirstKey, liveSecondKey: runLabel + "-" + liveSecondKey}
	t.Cleanup(func() {
		for _, name := range volumes {
			if guest.volumeExists(t, name) {
				guest.removeVolume(t, name)
			}
		}
	})
	fixture := newLiveBridgeFixture(t, inputs)
	fixture.writeLiveRoot(t, liveRoot{TokenVariable: inputs.TokenVariable, RunLabel: runLabel, PauseMarker: "", Volumes: map[string]string{liveFirstKey: volumes[liveFirstKey]}})
	requireSuccess(t, fixture.mutate(t, "init", "-input=false"))
	t.Cleanup(func() {
		fixture.writeLiveRoot(t, liveRoot{TokenVariable: inputs.TokenVariable, RunLabel: runLabel, PauseMarker: "", Volumes: volumes})
		destroy := fixture.mutate(t, "destroy", "-auto-approve", "-input=false")
		if destroy.exitCode != 0 {
			t.Errorf("destroy the live test volumes: %s", destroy.stderr)
		}
		requireNoLiveSecret(t, inputs, destroy.stdout, destroy.stderr)
	})

	create := fixture.apply(t, "-auto-approve", "-input=false")
	requireSuccess(t, create)
	fixture.requireNoDockerBridgeDirectories(t)
	if !guest.volumeExists(t, volumes[liveFirstKey]) {
		t.Fatalf("the guest Docker daemon does not have the volume %s", volumes[liveFirstKey])
	}
	plan := fixture.mutate(t, "plan", "-input=false", "-detailed-exitcode")
	requireSuccess(t, plan)
	repeat := fixture.apply(t, "-auto-approve", "-input=false")
	requireSuccess(t, repeat)
	if !strings.Contains(repeat.stdout+repeat.stderr, "No changes.") {
		t.Fatalf("the repeated apply changed infrastructure: %s", repeat.stdout)
	}
	fixture.requireNoDockerBridgeDirectories(t)
	firstState := fixture.mutate(t, "state", "show", "-no-color", liveFirstAddress)
	requireSuccess(t, firstState)

	marker := filepath.Join(fixture.temporary, "pause-marker")
	fixture.writeLiveRoot(t, liveRoot{TokenVariable: inputs.TokenVariable, RunLabel: runLabel, PauseMarker: marker, Volumes: volumes})
	command, stdout, stderr := fixture.startLive(t, "apply", "-auto-approve", "-input=false")
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	waitForMarker(t, marker, exited)
	if len(fixture.liveSockets(t)) == 0 {
		t.Fatal("the paused apply does not have a Docker bridge socket")
	}
	if !guest.succeeds(t, "test", "-e", liveLockFile) {
		t.Fatalf("the paused apply does not hold the guest lock %s", liveLockFile)
	}
	pidText, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	pausePID, err := strconv.Atoi(strings.TrimSpace(string(pidText)))
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
			t.Fatalf("the interrupted apply did not exit with a failure: %v", err)
		}
	case <-time.After(liveExitWait):
		t.Fatalf("configsctl did not exit within %s after the interruption", liveExitWait)
	}
	if err := syscall.Kill(pausePID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the OpenTofu provisioner process %d still runs after the interruption: %v", pausePID, err)
	}
	fixture.requireNoDockerBridgeDirectories(t)
	if guest.succeeds(t, "test", "-e", liveLockFile) {
		t.Fatalf("the interrupted apply left the guest lock %s", liveLockFile)
	}
	if !guest.volumeExists(t, volumes[liveFirstKey]) {
		t.Fatalf("the interruption removed the volume %s", volumes[liveFirstKey])
	}
	afterState := fixture.mutate(t, "state", "show", "-no-color", liveFirstAddress)
	requireSuccess(t, afterState)
	if afterState.stdout != firstState.stdout {
		t.Fatalf("the interruption changed the state of the first volume:\nbefore:\n%s\nafter:\n%s", firstState.stdout, afterState.stdout)
	}
	state, err := os.ReadFile(fixture.stateFile())
	if err != nil {
		t.Fatal(err)
	}
	requireNoLiveSecret(t, inputs, create.stdout, create.stderr, plan.stdout, plan.stderr, repeat.stdout, repeat.stderr,
		stdout.String(), stderr.String(), string(state))
}
