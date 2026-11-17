// Package guestrefs checks guest identities across native workspaces.
package guestrefs

import (
	"fmt"
	"maps"
	"slices"

	"goodkind.io/configsctl/internal/tofulock"
)

type validationError string

func (e validationError) Error() string { return string(e) }

type guestIdentity struct {
	Node     string `json:"node"`
	VMID     int    `json:"vmid"`
	Hostname string `json:"hostname"`
}

func (guest guestIdentity) address() string { return fmt.Sprintf("%s/%d", guest.Node, guest.VMID) }

func (guest guestIdentity) validate(location string) error {
	if guest.Node == "" || guest.VMID <= 0 || guest.Hostname == "" {
		return validationError(location + " requires node, positive vmid, and hostname")
	}

	return nil
}

type workspace struct {
	lines       locations
	Source      string                       `json:"source"`
	Declared    map[string]guestIdentity     `json:"declared"`
	Services    map[string]guestIdentity     `json:"services"`
	Locks       []string                     `json:"locks"`
	StaticLocks map[string]string            `json:"static_locks"`
	KeyedLocks  map[string]map[string]string `json:"keyed_locks"`
	Targets     tofulock.Targets             `json:"-"`
}

type guestDeclaration struct {
	identity guestIdentity
	location string
}

// Validate checks service references and locks against managed guests.
//
// A declaration must identify one service key, node and VMID, and hostname.
//
// Example:
//
//	err := registry.Validate()
func (workspaces Registry) Validate() error {
	declarations := make(map[string]guestDeclaration)
	addresses := make(map[string]string)
	hostnames := make(map[string]string)

	selectors := slices.Sorted(maps.Keys(workspaces))
	for _, selector := range selectors {
		config := workspaces[selector]
		if config.Source == "" || config.Declared == nil || config.Services == nil ||
			config.Locks == nil ||
			config.KeyedLocks == nil {
			return validationError(
				selector + " guest_references requires source, declared, services, locks, and keyed_locks",
			)
		}

		for _, key := range slices.Sorted(maps.Keys(config.Declared)) {
			guest := config.Declared[key]
			loc := fmt.Sprintf("%s managed_guests[%q]", config.location("managed_guests", key), key)
			if key == "" {
				return validationError(loc + " requires a service key")
			}

			if err := guest.validate(loc); err != nil {
				return err
			}

			if prior, found := declarations[key]; found {
				return validationError(fmt.Sprintf("%s duplicates %s", loc, prior.location))
			}

			if prior, found := addresses[guest.address()]; found {
				return validationError(
					fmt.Sprintf("%s conflicts with %s for %s", loc, prior, guest.address()),
				)
			}

			if prior, found := hostnames[guest.Hostname]; found {
				return validationError(
					fmt.Sprintf("%s conflicts with %s for hostname %s", loc, prior, guest.Hostname),
				)
			}

			declarations[key] = guestDeclaration{identity: guest, location: loc}
			addresses[guest.address()] = loc
			hostnames[guest.Hostname] = loc
		}
	}

	nodeHosts := workspaces.nodeHosts()

	for _, selector := range selectors {
		config := workspaces[selector]
		if err := config.validateStaticLocks(declarations, hostnames, nodeHosts); err != nil {
			return err
		}

		if err := config.validateServices(declarations); err != nil {
			return err
		}

		if err := config.validateLocks(declarations, hostnames); err != nil {
			return err
		}
	}

	return nil
}

func (config workspace) validateLocks(
	declarations map[string]guestDeclaration,
	hostnames map[string]string,
) error {
	expected := make(map[string]string)

	for _, key := range config.Locks {
		owner, found := declarations[key]
		if !found {
			return validationError(
				fmt.Sprintf(
					"%s guest locks reference undeclared guest %q",
					config.location("guest_locks"),
					key,
				),
			)
		}

		expected[owner.identity.address()] = owner.identity.Hostname
		if actual := config.Targets.GuestHosts[owner.identity.address()]; actual != owner.identity.Hostname {
			return validationError(
				fmt.Sprintf(
					"%s lock_targets.guest_hosts[%q] resolves to %q, expected %q from %s",
					config.location("lock_targets", "guest_hosts", owner.identity.address()),
					owner.identity.address(),
					actual,
					owner.identity.Hostname,
					owner.location,
				),
			)
		}
	}

	for _, address := range slices.Sorted(maps.Keys(config.Targets.GuestHosts)) {
		if _, found := expected[address]; !found {
			return validationError(
				fmt.Sprintf(
					"%s lock_targets.guest_hosts[%q] has no guest lock declaration",
					config.location("lock_targets", "guest_hosts", address),
					address,
				),
			)
		}
	}

	for _, module := range slices.Sorted(maps.Keys(config.KeyedLocks)) {
		for _, instance := range slices.Sorted(maps.Keys(config.KeyedLocks[module])) {
			key := config.KeyedLocks[module][instance]
			owner, found := declarations[key]
			loc := fmt.Sprintf(
				"%s lock_targets.module_key_hosts[%q][%q]",
				config.location("lock_targets", "module_key_hosts", module, instance),
				module,
				instance,
			)
			if !found {
				return validationError(
					fmt.Sprintf(
						"%s keyed_guest_locks[%q][%q] references undeclared guest %q",
						config.location("keyed_guest_locks", module, instance),
						module,
						instance,
						key,
					),
				)
			}

			if actual := config.Targets.ModuleKeyHosts[module][instance]; actual != owner.identity.Hostname {
				return validationError(
					fmt.Sprintf(
						"%s resolves to %q, expected %q from %s",
						loc,
						actual,
						owner.identity.Hostname,
						owner.location,
					),
				)
			}
		}
	}

	for _, module := range slices.Sorted(maps.Keys(config.Targets.ModuleKeyHosts)) {
		for _, instance := range slices.Sorted(maps.Keys(config.Targets.ModuleKeyHosts[module])) {
			host := config.Targets.ModuleKeyHosts[module][instance]
			_, guest := hostnames[host]
			if config.KeyedLocks[module][instance] == "" &&
				(guest || !config.isNodeHost(host)) {
				return validationError(
					fmt.Sprintf(
						"%s lock_targets.module_key_hosts[%q][%q] lacks the guest reference for %s",
						config.location("lock_targets", "module_key_hosts", module, instance),
						module,
						instance,
						host,
					),
				)
			}
		}
	}

	return nil
}

func (config workspace) validateServices(declarations map[string]guestDeclaration) error {
	for _, key := range slices.Sorted(maps.Keys(config.Services)) {
		guest := config.Services[key]
		loc := fmt.Sprintf("%s service_mapping[%q]", config.location("service_mapping", key), key)
		if err := guest.validate(loc); err != nil {
			return err
		}

		owner, found := declarations[key]
		if !found {
			return validationError(
				fmt.Sprintf(
					"%s references undeclared guest %s (%s)",
					loc,
					guest.Hostname,
					guest.address(),
				),
			)
		}

		if guest != owner.identity {
			return validationError(
				fmt.Sprintf(
					"%s references %s (%s), but %s declares %s (%s)",
					loc,
					guest.Hostname,
					guest.address(),
					owner.location,
					owner.identity.Hostname,
					owner.identity.address(),
				),
			)
		}
	}

	return nil
}

func (config workspace) isNodeHost(host string) bool {
	for _, name := range config.Targets.NodeHosts {
		if name == host {
			return true
		}
	}

	return false
}

func (workspaces Registry) nodeHosts() map[string]bool {
	hosts := make(map[string]bool)

	for _, config := range workspaces {
		for _, host := range config.Targets.NodeHosts {
			hosts[host] = true
		}
	}

	return hosts
}

func (config workspace) validateStaticLocks(
	declarations map[string]guestDeclaration,
	guestHosts map[string]string,
	nodeHosts map[string]bool,
) error {
	for _, address := range slices.Sorted(maps.Keys(config.StaticLocks)) {
		key := config.StaticLocks[address]
		owner, found := declarations[key]
		if !found {
			return validationError(
				fmt.Sprintf(
					"%s static_guest_locks[%q] references undeclared guest %q",
					config.location("static_guest_locks", address),
					address,
					key,
				),
			)
		}

		if actual := config.Targets.ModuleHosts[address]; actual != owner.identity.Hostname {
			return validationError(
				fmt.Sprintf(
					"%s lock_targets.module_hosts[%q] resolves to %q, expected %q from %s",
					config.location("lock_targets", "module_hosts", address),
					address,
					actual,
					owner.identity.Hostname,
					owner.location,
				),
			)
		}
	}

	for _, address := range slices.Sorted(maps.Keys(config.Targets.ModuleHosts)) {
		host := config.Targets.ModuleHosts[address]
		_, guest := guestHosts[host]
		if config.StaticLocks[address] == "" && (guest || !nodeHosts[host]) {
			return validationError(
				fmt.Sprintf(
					"%s lock_targets.module_hosts[%q] requires a guest reference or a declared node host for %q",
					config.location("lock_targets", "module_hosts", address),
					address,
					host,
				),
			)
		}
	}

	return nil
}
