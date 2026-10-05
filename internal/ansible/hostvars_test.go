package ansible

import (
	"strings"
	"testing"
)

// inventoryOutput follows the ansible-core 2.19 inventory encoder. The encoder
// writes an untrusted host variable, such as the ansible_host that the Proxmox
// inventory plugin composes, as an __ansible_unsafe object.
const inventoryOutput = `{
    "_meta": {
        "hostvars": {
            "adguard.home.goodkind.io": {
                "ansible_host": {"__ansible_unsafe": "3d06:bad:b01::53"},
                "ansible_proxmox_vmid": 112
            },
            "localhost": {"ansible_connection": "local"},
            "vault": {"ansible_host": "3d06:bad:b01::254", "ansible_user": "root"}
        }
    },
    "proxmox_servers": {"children": ["vault_servers"]},
    "vault_servers": {"hosts": ["vault"]}
}`

func TestDecodeInventoryReadsUntrustedConnectionVariables(t *testing.T) {
	inv, err := decodeInventory([]byte(inventoryOutput))
	if err != nil {
		t.Fatalf("decodeInventory: %v", err)
	}
	want := map[string]HostVars{
		"adguard.home.goodkind.io": {AnsibleHost: "3d06:bad:b01::53", AnsibleUser: "", AnsibleConnection: ""},
		"localhost":                {AnsibleHost: "", AnsibleUser: "", AnsibleConnection: "local"},
		"vault":                    {AnsibleHost: "3d06:bad:b01::254", AnsibleUser: "root", AnsibleConnection: ""},
	}
	for host, vars := range want {
		if inv.Hosts[host] != vars {
			t.Fatalf("Hosts[%s] = %+v, want %+v", host, inv.Hosts[host], vars)
		}
	}
	if got := inv.GroupHosts("proxmox_servers"); len(got) != 1 || got[0] != "vault" {
		t.Fatalf("GroupHosts(proxmox_servers) = %v, want [vault]", got)
	}
}

func TestDecodeInventoryRefusesAnEncryptedConnectionVariable(t *testing.T) {
	out := `{"_meta": {"hostvars": {"vault": {"ansible_user": {"__ansible_vault": "$ANSIBLE_VAULT;1.1;AES256"}}}}}`
	_, err := decodeInventory([]byte(out))
	if err == nil || !strings.Contains(err.Error(), "ansible_user") {
		t.Fatalf("decodeInventory: err = %v, want a refusal that names ansible_user", err)
	}
}
