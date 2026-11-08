package dockeroverlay_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"text/template"
)

const (
	providerSource       = "registry.opentofu.org/kreuzwerker/docker"
	providerPackage      = "github.com/terraform-providers/terraform-provider-docker"
	providerBinary       = "terraform-provider-docker"
	overlayVersion       = "4.6.0-configsctl.1"
	stockVersion         = "4.6.0"
	ipv4Host             = "ipv4.extra-hosts.test"
	ipv4Address          = "192.0.2.10"
	ipv6Host             = "ipv6.extra-hosts.test"
	ipv6Address          = "2001:db8:bad:b01:210::220"
	stockIPv6Readback    = "2001"
	containerAddress     = "docker_container.probe"
	containerNamePrefix  = "configsctl-docker-overlay-"
	containerNameEntropy = 6
	planClean            = 0
	planChanged          = 2
	fixtureDirectory     = "testdata/extrahosts"
)

type rootTemplateData struct {
	ProviderVersion string
	IPv4Host        string
	IPv4Address     string
	IPv6Host        string
	IPv6Address     string
}

type mirrorTemplateData struct {
	Mirror string
	Source string
}

type tofuRoot struct {
	directory     string
	environment   []string
	containerName string
}

type tofuState struct {
	Values struct {
		RootModule struct {
			Resources []tofuResource `json:"resources"`
		} `json:"root_module"`
	} `json:"values"`
}

type tofuResource struct {
	Address string `json:"address"`
	Values  struct {
		ID    string      `json:"id"`
		Hosts []extraHost `json:"host"`
	} `json:"values"`
}

type extraHost struct {
	Host string `json:"host"`
	IP   string `json:"ip"`
}

// TestExtraHostsSurviveRefresh runs OpenTofu against the local Docker daemon
// and requires tofu and docker on PATH. The patched provider must retain both
// extra host addresses across refresh and preserve the container on repeat
// apply. The stock provider truncates the IPv6 address to 2001 and plans a
// replacement, confirming that the fixture reproduces the defect. Each subtest
// destroys its resources; the test fails and force removes any remaining
// container.
func TestExtraHostsSurviveRefresh(t *testing.T) {
	requireTool(t, "tofu")
	requireTool(t, "docker")
	dockerHost := dockerEndpoint(t)

	t.Run("patched provider keeps both addresses", func(t *testing.T) {
		mirror := buildPatchedProvider(t)
		cliConfig := renderFile(t, "mirror.tfrc.tmpl", mirrorTemplateData{Mirror: mirror, Source: providerSource})
		root := newTofuRoot(t, overlayVersion, cliConfig, dockerHost)
		root.run(t, "init", "-input=false")
		root.run(t, "apply", "-input=false", "-auto-approve")
		root.run(t, "apply", "-input=false", "-auto-approve", "-refresh-only")
		created := root.container(t)
		requireHosts(t, created.Values.Hosts, ipv6Address)
		if exitCode := root.plan(t); exitCode != planClean {
			t.Fatalf("repeat plan exit code = %d, want %d", exitCode, planClean)
		}
		t.Logf("second tofu apply:\n%s", root.run(t, "apply", "-input=false", "-auto-approve"))
		reapplied := root.container(t)
		if reapplied.Values.ID != created.Values.ID {
			t.Fatalf("second apply replaced container %s with %s", created.Values.ID, reapplied.Values.ID)
		}
		requireHosts(t, reapplied.Values.Hosts, ipv6Address)
	})

	t.Run("stock provider truncates the IPv6 address", func(t *testing.T) {
		cliConfig := filepath.Join(fixtureDirectory, "registry.tfrc")
		absoluteConfig, err := filepath.Abs(cliConfig)
		if err != nil {
			t.Fatal(err)
		}
		root := newTofuRoot(t, stockVersion, absoluteConfig, dockerHost)
		root.run(t, "init", "-input=false")
		root.run(t, "apply", "-input=false", "-auto-approve")
		root.run(t, "apply", "-input=false", "-auto-approve", "-refresh-only")
		requireHosts(t, root.container(t).Values.Hosts, stockIPv6Readback)
		if exitCode := root.plan(t); exitCode != planChanged {
			t.Fatalf("repeat plan exit code = %d, want %d", exitCode, planChanged)
		}
	})
}

func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Fatalf("install %s and add it to PATH before running this test: %v", name, err)
	}
}

func dockerEndpoint(t *testing.T) string {
	t.Helper()
	output := runCommand(t, "", nil, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	endpoint := strings.TrimSpace(output)
	if endpoint == "" {
		t.Fatal("docker context inspect did not report a daemon endpoint")
	}
	return endpoint
}

func buildPatchedProvider(t *testing.T) string {
	t.Helper()
	mirror := t.TempDir()
	platform := runtime.GOOS + "_" + runtime.GOARCH
	packageDirectory := filepath.Join(mirror, filepath.FromSlash(providerSource), overlayVersion, platform)
	binaryPath := filepath.Join(packageDirectory, providerBinary+"_v"+overlayVersion)
	runCommand(t, "", nil, "go", "build", "-o", binaryPath, providerPackage)
	return mirror
}

func newTofuRoot(t *testing.T, providerVersion, cliConfig, dockerHost string) *tofuRoot {
	t.Helper()
	directory := t.TempDir()
	content := renderTemplate(t, "main.tf.tmpl", rootTemplateData{
		ProviderVersion: providerVersion,
		IPv4Host:        ipv4Host,
		IPv4Address:     ipv4Address,
		IPv6Host:        ipv6Host,
		IPv6Address:     ipv6Address,
	})
	if err := os.WriteFile(filepath.Join(directory, "main.tf"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	root := &tofuRoot{
		directory:     directory,
		containerName: containerNamePrefix + randomSuffix(t),
		environment: append(os.Environ(),
			"TF_CLI_CONFIG_FILE="+cliConfig,
			"TF_IN_AUTOMATION=1",
			"DOCKER_HOST="+dockerHost,
		),
	}
	t.Cleanup(func() { root.destroy(t) })
	return root
}

func (root *tofuRoot) run(t *testing.T, arguments ...string) string {
	t.Helper()
	return runCommand(t, root.directory, root.environment, "tofu", root.withVariables(arguments)...)
}

func (root *tofuRoot) withVariables(arguments []string) []string {
	switch arguments[0] {
	case "apply", "plan", "destroy":
		return append(arguments, "-no-color", "-var", "container_name="+root.containerName)
	default:
		return append(arguments, "-no-color")
	}
}

func (root *tofuRoot) command(name string, arguments ...string) *exec.Cmd {
	command := exec.CommandContext(context.Background(), name, arguments...)
	command.Dir = root.directory
	command.Env = root.environment
	return command
}

func (root *tofuRoot) plan(t *testing.T) int {
	t.Helper()
	output, err := root.command("tofu", root.withVariables([]string{"plan", "-input=false", "-detailed-exitcode"})...).CombinedOutput()
	t.Logf("tofu plan -detailed-exitcode:\n%s", output)
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	if err != nil {
		t.Fatalf("tofu plan did not run: %v", err)
	}
	return planClean
}

func (root *tofuRoot) container(t *testing.T) tofuResource {
	t.Helper()
	var state tofuState
	if err := json.Unmarshal([]byte(root.run(t, "show", "-json")), &state); err != nil {
		t.Fatalf("decode tofu show -json: %v", err)
	}
	for _, resource := range state.Values.RootModule.Resources {
		if resource.Address == containerAddress {
			return resource
		}
	}
	t.Fatalf("state does not contain %s", containerAddress)
	return tofuResource{}
}

func (root *tofuRoot) destroy(t *testing.T) {
	t.Helper()
	destroy := root.command("tofu", root.withVariables([]string{"destroy", "-input=false", "-auto-approve"})...)
	if output, err := destroy.CombinedOutput(); err != nil {
		t.Errorf("tofu destroy failed: %v\n%s", err, output)
	}
	if root.command("docker", "container", "inspect", root.containerName).Run() != nil {
		return
	}
	t.Errorf("container %s remained after tofu destroy", root.containerName)
	remove := root.command("docker", "container", "rm", "--force", root.containerName)
	if output, err := remove.CombinedOutput(); err != nil {
		t.Errorf("docker container rm %s failed: %v\n%s", root.containerName, err, output)
	}
}

func requireHosts(t *testing.T, hosts []extraHost, wantIPv6 string) {
	t.Helper()
	want := map[string]string{ipv4Host: ipv4Address, ipv6Host: wantIPv6}
	got := make(map[string]string, len(hosts))
	for _, host := range hosts {
		got[host.Host] = host.IP
	}
	if !maps.Equal(got, want) {
		t.Fatalf("state hosts = %v, want %v", got, want)
	}
}

func renderTemplate(t *testing.T, name string, data any) []byte {
	t.Helper()
	parsed, err := template.ParseFiles(filepath.Join(fixtureDirectory, name))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := parsed.Execute(&output, data); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func renderFile(t *testing.T, name string, data any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), strings.TrimSuffix(name, ".tmpl"))
	if err := os.WriteFile(path, renderTemplate(t, name, data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	entropy := make([]byte, containerNameEntropy)
	if _, err := rand.Read(entropy); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(entropy)
}

func runCommand(t *testing.T, directory string, environment []string, name string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(context.Background(), name, arguments...)
	command.Dir = directory
	command.Env = environment
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("%s %s failed: %v\nstdout:\n%s\nstderr:\n%s", name, strings.Join(arguments, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}
