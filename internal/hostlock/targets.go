package hostlock

import "goodkind.io/configsctl/internal/ansible"

type connection string

// sshConnections lists the ansible_connection values that use ssh. An empty
// value is the Ansible default, ssh.
var sshConnections = map[connection]bool{"": true, "ssh": true, "smart": true}

// Targets returns SSH lock targets and a local lock target when a local
// inventory host matches hostname. It also returns the hosts it excludes.
// Callers must read this machine's hostname before selecting local hosts.
func Targets(inv ansible.Inventory, names []string, hostname string) (hosts []Host, skipped []string) {
	hosts = make([]Host, 0, len(names))
	for _, name := range names {
		vars := inv.Hosts[name]
		if vars.AnsibleConnection == "local" {
			if name == hostname {
				hosts = append(hosts, Host{Name: name, User: "", Address: "", Dir: DefaultDir})
			} else {
				skipped = append(skipped, name)
			}
			continue
		}
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
	return hosts, skipped
}
