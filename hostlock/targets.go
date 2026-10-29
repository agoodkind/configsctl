package hostlock

import (
	"log/slog"

	"goodkind.io/configsctl/internal/ansible"
)

type connection string

// sshConnections lists the ansible_connection values that use ssh. An empty
// value is the Ansible default, ssh.
var sshConnections = map[connection]bool{"": true, "ssh": true, "smart": true}

// Targets returns a lock target for each named host that Ansible connects to
// over ssh. A host with any other connection type, such as local, gets no
// target.
func Targets(inv ansible.Inventory, names []string) []Host {
	hosts := make([]Host, 0, len(names))
	var skipped []string
	for _, name := range names {
		vars := inv.Hosts[name]
		if !sshConnections[connection(vars.AnsibleConnection)] {
			skipped = append(skipped, name)
			continue
		}
		address := vars.AnsibleHost
		if address == "" {
			address = name
		}
		user := vars.AnsibleUser
		if user == "" {
			user = "root"
		}
		hosts = append(hosts, Host{Name: name, User: user, Address: address, Dir: DefaultDir})
	}
	if len(skipped) > 0 {
		slog.Info("hostlock.targets_skipped", "hosts", skipped)
	}
	return hosts
}
