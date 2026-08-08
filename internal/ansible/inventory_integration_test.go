package ansible

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const ansiblePlaybookTimeout = 30 * time.Second

type renderedInventoryHost struct {
	AnsibleHost          string `json:"ansible_host"`
	AnsibleSSHCommonArgs string `json:"ansible_ssh_common_args"`
	SSHBaseArgs          string `json:"ssh_base_args"`
	RouterTransit        string `json:"router_ipv6_transit"`
}

func copyInventoryFile(t *testing.T, source string, destination string) {
	t.Helper()
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", source, err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(destination), err)
	}
	if err := os.WriteFile(destination, contents, 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", destination, err)
	}
}

func testInventoryDirectory(t *testing.T, repositoryRoot string) string {
	t.Helper()
	inventoryDirectory := filepath.Join(t.TempDir(), "inventory")
	inputPaths := []string{
		"service_mapping.yml",
		"group_vars/all/service_mapping.yml",
		"group_vars/all/vars.yml",
	}
	for _, inputPath := range inputPaths {
		copyInventoryFile(
			t,
			filepath.Join(repositoryRoot, "ansible", "inventory", inputPath),
			filepath.Join(inventoryDirectory, inputPath),
		)
	}
	return inventoryDirectory
}

func usesIndirectSSH(arguments string) bool {
	lowerArguments := strings.ToLower(arguments)
	if strings.Contains(lowerArguments, "proxyjump") {
		return true
	}
	if strings.Contains(lowerArguments, "proxycommand") {
		return true
	}
	for _, field := range strings.Fields(arguments) {
		if field == "-J" || strings.HasPrefix(field, "-J") {
			return true
		}
	}
	return false
}

func isRoutedIPv6(address string) bool {
	parsedAddress, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	return parsedAddress.Is6() &&
		!parsedAddress.Is4In6() &&
		parsedAddress.IsGlobalUnicast()
}

func TestUsesIndirectSSH(t *testing.T) {
	testCases := []struct {
		name      string
		arguments string
		want      bool
	}{
		{name: "direct", arguments: "-o StrictHostKeyChecking=no", want: false},
		{name: "ProxyJump", arguments: "-o ProxyJump=proxy", want: true},
		{name: "attached ProxyJump", arguments: "-oProxyJump=proxy", want: true},
		{name: "separate short option", arguments: "-J proxy", want: true},
		{name: "attached short option", arguments: "-Jproxy", want: true},
		{name: "ProxyCommand", arguments: "-o ProxyCommand='ssh proxy'", want: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got := usesIndirectSSH(testCase.arguments)
			if got != testCase.want {
				t.Errorf("usesIndirectSSH(%q) = %t, want %t", testCase.arguments, got, testCase.want)
			}
		})
	}
}

func TestIsRoutedIPv6(t *testing.T) {
	testCases := []struct {
		name    string
		address string
		want    bool
	}{
		{name: "routed IPv6", address: "3d06:bad:b01:201::2", want: true},
		{name: "IPv4", address: "192.0.2.1", want: false},
		{name: "mapped IPv4", address: "::ffff:192.0.2.1", want: false},
		{name: "link local IPv6", address: "fe80::1", want: false},
		{name: "hostname", address: "router.suburban.goodkind.io", want: false},
		{name: "empty", address: "", want: false},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got := isRoutedIPv6(testCase.address)
			if got != testCase.want {
				t.Errorf("isRoutedIPv6(%q) = %t, want %t", testCase.address, got, testCase.want)
			}
		})
	}
}

func TestSuburbanInventoryUsesDirectSSH(t *testing.T) {
	ansiblePlaybook, err := exec.LookPath("ansible-playbook")
	if err != nil {
		t.Fatalf("ansible-playbook is required: %v", err)
	}

	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	repositoryRoot := filepath.Clean(filepath.Join(workingDirectory, "..", "..", ".."))
	inventoryDirectory := testInventoryDirectory(t, repositoryRoot)
	renderedDirectory := t.TempDir()
	vaultPasswordFile := filepath.Join(t.TempDir(), "vault-password")
	if err := os.WriteFile(vaultPasswordFile, []byte("unused\n"), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", vaultPasswordFile, err)
	}
	extraVariables, err := json.Marshal(map[string]any{
		"output_directory": renderedDirectory,
		"ansible_become":   false,
	})
	if err != nil {
		t.Fatalf("encode extra variables: %v", err)
	}

	commandContext, cancel := context.WithTimeout(t.Context(), ansiblePlaybookTimeout)
	defer cancel()
	command := exec.CommandContext(
		commandContext,
		ansiblePlaybook,
		"--inventory",
		inventoryDirectory,
		filepath.Join(workingDirectory, "testdata", "render_inventory.yml"),
		"--extra-vars",
		string(extraVariables),
	)
	command.Dir = filepath.Join(repositoryRoot, "ansible")
	command.Env = append(
		os.Environ(),
		"ANSIBLE_VAULT_PASSWORD_FILE="+vaultPasswordFile,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		if commandContext.Err() != nil {
			t.Fatalf(
				"render inventory exceeded %s: %v\n%s",
				ansiblePlaybookTimeout,
				commandContext.Err(),
				output,
			)
		}
		t.Fatalf("render inventory: %v\n%s", err, output)
	}

	expectedHosts := []string{
		"tack-qa.suburban.goodkind.io",
		"seaweedfs.suburban.goodkind.io",
		"tack-gh-runner.suburban.goodkind.io",
		"dns64.suburban.goodkind.io",
		"mwan.suburban.goodkind.io",
		"mwan-failover.suburban.goodkind.io",
		"router.suburban.goodkind.io",
	}
	for _, hostname := range expectedHosts {
		t.Run(hostname, func(t *testing.T) {
			path := filepath.Join(renderedDirectory, hostname+".json")
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile %s: %v", path, err)
			}
			var rendered renderedInventoryHost
			if err := json.Unmarshal(contents, &rendered); err != nil {
				t.Fatalf("decode %s: %v", path, err)
			}
			if rendered.AnsibleSSHCommonArgs != rendered.SSHBaseArgs {
				t.Errorf(
					"ansible_ssh_common_args = %q, want shared ssh_base_args %q",
					rendered.AnsibleSSHCommonArgs,
					rendered.SSHBaseArgs,
				)
			}
			if usesIndirectSSH(rendered.AnsibleSSHCommonArgs) {
				t.Errorf(
					"SSH args use an indirect connection: %q",
					rendered.AnsibleSSHCommonArgs,
				)
			}
			if hostname == "router.suburban.goodkind.io" {
				if !isRoutedIPv6(rendered.AnsibleHost) {
					t.Errorf(
						"router ansible_host = %q, want routed IPv6 literal",
						rendered.AnsibleHost,
					)
				}
				if rendered.AnsibleHost != rendered.RouterTransit {
					t.Errorf(
						"router ansible_host = %q, want routed IPv6 %q",
						rendered.AnsibleHost,
						rendered.RouterTransit,
					)
				}
			}
		})
	}
}
