package ansible_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/ansible"
)

// hostVarsOutput follows the ansible-core 2.19 inventory encoder for
// _meta.hostvars. The encoder writes an untrusted host variable, such as the
// ansible_host that the Proxmox inventory plugin composes, as an
// __ansible_unsafe object.
const hostVarsOutput = `{
    "adguard.home.goodkind.io": {
        "ansible_host": {"__ansible_unsafe": "3d06:bad:b01::53"},
        "ansible_proxmox_vmid": 112
    },
    "localhost": {"ansible_connection": "local"},
    "vault": {"ansible_host": "3d06:bad:b01::254", "ansible_user": "root"}
}`

func TestHostVarsReadsUntrustedConnectionVariables(t *testing.T) {
	var hosts map[string]ansible.HostVars
	if err := json.Unmarshal([]byte(hostVarsOutput), &hosts); err != nil {
		t.Fatalf("decode hostvars: %v", err)
	}
	want := map[string]ansible.HostVars{
		"adguard.home.goodkind.io": {AnsibleHost: "3d06:bad:b01::53", AnsibleUser: "", AnsibleConnection: ""},
		"localhost":                {AnsibleHost: "", AnsibleUser: "", AnsibleConnection: "local"},
		"vault":                    {AnsibleHost: "3d06:bad:b01::254", AnsibleUser: "root", AnsibleConnection: ""},
	}
	for host, vars := range want {
		if hosts[host] != vars {
			t.Fatalf("hostvars[%s] = %+v, want %+v", host, hosts[host], vars)
		}
	}
}

const delegateInventoryOutput = `{
    "_meta": {
        "hostvars": {
            "mwan": {"mwan_proxmox_delegate": "vault"},
            "mwan-suburban": {"mwan_proxmox_delegate": {"__ansible_unsafe": "suburban"}},
            "opnsense": {"ansible_host": "3d06:bad:b01::1"},
            "suburban": {"ansible_user": "root"},
            "vault": {"ansible_host": "3d06:bad:b01::254"}
        }
    },
    "mwan_servers": {"hosts": ["mwan"]},
    "mwan_suburban_servers": {"hosts": ["mwan-suburban"]}
}`

const delegateVariable = "mwan_proxmox_delegate"

func decodeDelegateInventory(t *testing.T) ansible.Inventory {
	t.Helper()
	inv, err := ansible.DecodeInventory([]byte(delegateInventoryOutput))
	if err != nil {
		t.Fatalf("decode inventory: %v", err)
	}
	return inv
}

func TestVariableHostsReadsPlainAndUntrustedHostNames(t *testing.T) {
	inv := decodeDelegateInventory(t)

	hosts, err := inv.VariableHosts([]string{"mwan", "mwan-suburban"}, []string{delegateVariable})
	if err != nil {
		t.Fatalf("VariableHosts: %v", err)
	}
	if want := []string{"suburban", "vault"}; !slices.Equal(hosts, want) {
		t.Fatalf("VariableHosts = %v, want %v", hosts, want)
	}
}

func TestVariableHostsRefusesAHostWithoutTheVariable(t *testing.T) {
	inv := decodeDelegateInventory(t)

	_, err := inv.VariableHosts([]string{"mwan", "opnsense"}, []string{delegateVariable})
	if err == nil || !strings.Contains(err.Error(), "opnsense") || !strings.Contains(err.Error(), delegateVariable) {
		t.Fatalf("VariableHosts err = %v, want a refusal that includes opnsense and %s", err, delegateVariable)
	}
}

func TestPlaybookNameMatchesEveryDeployForm(t *testing.T) {
	for _, playbook := range []string{"deploy-mwan", "deploy-mwan.yml", "playbooks/deploy-mwan.yml"} {
		if got := ansible.PlaybookName(playbook); got != "deploy-mwan" {
			t.Fatalf("PlaybookName(%q) = %q, want deploy-mwan", playbook, got)
		}
	}
}

func TestHostVarsRefusesAnEncryptedOrNullWrappedConnectionVariable(t *testing.T) {
	outputs := []string{
		`{"vault": {"ansible_user": {"__ansible_vault": "$ANSIBLE_VAULT;1.1;AES256"}}}`,
		`{"vault": {"ansible_user": {"__ansible_unsafe": null}}}`,
	}
	for _, out := range outputs {
		var hosts map[string]ansible.HostVars
		err := json.Unmarshal([]byte(out), &hosts)
		if err == nil || !strings.Contains(err.Error(), "ansible_user") {
			t.Fatalf("decode %s: err = %v, want a refusal that includes ansible_user", out, err)
		}
	}
}
