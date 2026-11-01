// Package tofulock selects inventory hosts to lock from saved OpenTofu plans.
package tofulock

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"goodkind.io/configsctl/hostlock"
)

// defaultLockUser is the ssh user of a lock endpoint that declares no user.
const defaultLockUser = "root"

// Targets declares inventory hosts for a workspace's modules, nodes, and guests.
type Targets struct {
	ModuleHosts    map[string]string            `yaml:"module_hosts"`
	ModuleKeyHosts map[string]map[string]string `yaml:"module_key_hosts"`
	NodeHosts      map[string]string            `yaml:"node_hosts"`
	GuestHosts     map[string]string            `yaml:"guest_hosts"`
	LockFreeTypes  []string                     `yaml:"lock_free_types"`
	// LockEndpoints maps each lock host name to its ssh endpoint. A workspace
	// that declares it locks without reading the Ansible inventory.
	LockEndpoints map[string]LockEndpoint `yaml:"lock_endpoints"`
}

// LockEndpoint is the ssh endpoint of one lock host. User defaults to root.
type LockEndpoint struct {
	User    string `yaml:"user"`
	Address string `yaml:"address"`
}

// Validate requires a host mapping or declared lock-free resource types.
// Validate rejects empty declared maps, keys, and host names.
// Validate requires guest keys in <node>/<vmid> form.
// Validate rejects a declared LockFreeTypes list with no types or an empty name.
// Validate requires each lock endpoint to have an address and a host name that a
// host mapping uses. When any endpoint is declared, Validate requires an
// endpoint for each host in the host mappings.
func (t Targets) Validate() error {
	if !t.hasDeclarations() {
		return errors.New("declares no mappings")
	}
	if err := validateHostMap("module_hosts", t.ModuleHosts); err != nil {
		return err
	}
	if err := validateHostMap("node_hosts", t.NodeHosts); err != nil {
		return err
	}
	if err := validateHostMap("guest_hosts", t.GuestHosts); err != nil {
		return err
	}
	if err := validateGuestKeys(t.GuestHosts); err != nil {
		return err
	}
	if t.LockFreeTypes != nil && len(t.LockFreeTypes) == 0 {
		return errors.New("lock_free_types is empty")
	}
	if slices.Contains(t.LockFreeTypes, "") {
		return errors.New("lock_free_types has an empty type name")
	}
	if t.ModuleKeyHosts != nil && len(t.ModuleKeyHosts) == 0 {
		return errors.New("module_key_hosts is empty")
	}
	for _, module := range slices.Sorted(maps.Keys(t.ModuleKeyHosts)) {
		if module == "" {
			return errors.New("module_key_hosts has an empty module address")
		}
		mapping := t.ModuleKeyHosts[module]
		if mapping == nil {
			return fmt.Errorf("module_key_hosts.%s is empty", module)
		}
		if err := validateHostMap("module_key_hosts."+module, mapping); err != nil {
			return err
		}
	}
	return t.validateLockEndpoints()
}

func (t Targets) validateLockEndpoints() error {
	if t.LockEndpoints == nil {
		return nil
	}
	if len(t.LockEndpoints) == 0 {
		return errors.New("lock_endpoints is empty")
	}
	for _, name := range slices.Sorted(maps.Keys(t.LockEndpoints)) {
		if name == "" {
			return errors.New("lock_endpoints has an empty host name")
		}
		if t.LockEndpoints[name].Address == "" {
			return fmt.Errorf("lock_endpoints.%s has an empty address; a local lock is not supported", name)
		}
	}
	referenced := t.referencedHosts()
	for _, name := range slices.Sorted(maps.Keys(t.LockEndpoints)) {
		if _, used := referenced[name]; !used {
			return fmt.Errorf("lock_endpoints.%s is not a host in module_hosts, module_key_hosts, node_hosts, or guest_hosts", name)
		}
	}
	for _, host := range slices.Sorted(maps.Keys(referenced)) {
		if _, declared := t.LockEndpoints[host]; !declared {
			return fmt.Errorf("lock_endpoints does not declare host %s from %s", host, referenced[host])
		}
	}
	return nil
}

// referencedHosts maps each host name in the host mappings to the first
// mapping that uses it.
func (t Targets) referencedHosts() map[string]string {
	referenced := map[string]string{}
	add := func(source string, mapping map[string]string) {
		for _, key := range slices.Sorted(maps.Keys(mapping)) {
			if _, seen := referenced[mapping[key]]; !seen {
				referenced[mapping[key]] = source + "." + key
			}
		}
	}
	add("module_hosts", t.ModuleHosts)
	for _, module := range slices.Sorted(maps.Keys(t.ModuleKeyHosts)) {
		add("module_key_hosts."+module, t.ModuleKeyHosts[module])
	}
	add("node_hosts", t.NodeHosts)
	add("guest_hosts", t.GuestHosts)
	return referenced
}

// LockHosts returns the ssh lock target of each host and guest lock in
// decision. It reads only LockEndpoints and returns an error for a host
// without an endpoint.
func (t Targets) LockHosts(decision Decision) ([]hostlock.Host, error) {
	var hosts []hostlock.Host
	for _, name := range decision.Hosts {
		host, err := t.lockHost(name)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	for _, guest := range decision.GuestLocks {
		if err := guest.Validate(); err != nil {
			slog.Error("tofu.lock.guest_identity_invalid", "node", guest.Node, "err", err)
			return nil, fmt.Errorf("validate the guest lock identity: %w", err)
		}
		host, err := t.lockHost(guest.Host)
		if err != nil {
			return nil, err
		}
		guestHost, err := hostlock.ForGuest(host, guest.Node, guest.VMID)
		if err != nil {
			slog.Error("tofu.lock.guest_target_failed", "node", guest.Node, "err", err)
			return nil, fmt.Errorf("select the guest lock target: %w", err)
		}
		hosts = append(hosts, guestHost)
	}
	return hosts, nil
}

func (t Targets) lockHost(name string) (hostlock.Host, error) {
	endpoint, declared := t.LockEndpoints[name]
	if !declared {
		return hostlock.Host{}, fmt.Errorf("host %s has no declared lock endpoint", name)
	}
	user := endpoint.User
	if user == "" {
		user = defaultLockUser
	}
	return hostlock.Host{Name: name, User: user, Address: endpoint.Address, Dir: hostlock.DefaultDir}, nil
}

func (t Targets) hasDeclarations() bool {
	return t.ModuleHosts != nil || t.ModuleKeyHosts != nil || t.NodeHosts != nil || t.GuestHosts != nil || t.LockFreeTypes != nil
}

func validateHostMap(name string, mapping map[string]string) error {
	if mapping != nil && len(mapping) == 0 {
		return fmt.Errorf("%s is empty", name)
	}
	for _, key := range slices.Sorted(maps.Keys(mapping)) {
		if key == "" {
			return fmt.Errorf("%s has an empty key", name)
		}
		if mapping[key] == "" {
			return fmt.Errorf("%s.%s has an empty host name", name, key)
		}
	}
	return nil
}

func validateGuestKeys(mapping map[string]string) error {
	for _, key := range slices.Sorted(maps.Keys(mapping)) {
		node, vmid, found := strings.Cut(key, "/")
		if !found || node == "" || vmid == "" {
			return fmt.Errorf("guest_hosts key %q is not <node>/<vmid>", key)
		}
	}
	return nil
}
