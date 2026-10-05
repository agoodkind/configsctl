package ansible_test

import (
	"encoding/json"
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

func TestHostVarsRefusesAnEncryptedConnectionVariable(t *testing.T) {
	out := `{"vault": {"ansible_user": {"__ansible_vault": "$ANSIBLE_VAULT;1.1;AES256"}}}`
	var hosts map[string]ansible.HostVars
	err := json.Unmarshal([]byte(out), &hosts)
	if err == nil || !strings.Contains(err.Error(), "ansible_user") {
		t.Fatalf("decode hostvars: err = %v, want a refusal that includes ansible_user", err)
	}
}
