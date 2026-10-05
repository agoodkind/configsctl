package hostlock

import (
	"log/slog"

	"goodkind.io/configsctl/internal/ansible"
)

// Targets returns a lock target for each named host that Ansible connects to
// over ssh. A host with any other connection type, such as local, gets no
// target.
func Targets(inv ansible.Inventory, names []string) []Host {
	hosts := make([]Host, 0, len(names))
	for _, name := range names {
		vars := inv.Hosts[name]
		switch vars.AnsibleConnection {
		case "", "ssh", "smart":
		default:
			slog.Info("hostlock.target_skipped", "host", name, "connection", vars.AnsibleConnection)
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
	return hosts
}
