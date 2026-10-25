package hostlock

import (
	"fmt"
	"log/slog"
	"os"

	"goodkind.io/configsctl/internal/ansible"
)

type connection string

// sshConnections lists the ansible_connection values that use ssh. An empty
// value is the Ansible default, ssh.
var sshConnections = map[connection]bool{"": true, "ssh": true, "smart": true}

// Targets returns SSH lock targets and a local lock target when a local
// inventory host matches this machine's hostname. It excludes other local
// hosts and unsupported connection types.
func Targets(inv ansible.Inventory, names []string) ([]Host, error) {
	hostname, err := localHostname(inv, names)
	if err != nil {
		return nil, err
	}
	hosts := make([]Host, 0, len(names))
	var skipped []string
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
	if len(skipped) > 0 {
		slog.Info("hostlock.targets_skipped", "hosts", skipped)
	}
	return hosts, nil
}

func localHostname(inv ansible.Inventory, names []string) (string, error) {
	for _, name := range names {
		if inv.Hosts[name].AnsibleConnection != "local" {
			continue
		}
		hostname, err := os.Hostname()
		if err != nil {
			slog.Error("hostlock.hostname_unavailable", "err", err)
			return "", fmt.Errorf("read the hostname for local host locks: %w", err)
		}
		return hostname, nil
	}
	return "", nil
}
