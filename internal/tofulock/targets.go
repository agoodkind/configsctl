// Package tofulock selects inventory hosts to lock from saved OpenTofu plans.
package tofulock

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Targets declares inventory hosts for a workspace's modules, nodes, and guests.
type Targets struct {
	ModuleHosts    map[string]string            `yaml:"module_hosts"`
	ModuleKeyHosts map[string]map[string]string `yaml:"module_key_hosts"`
	NodeHosts      map[string]string            `yaml:"node_hosts"`
	GuestHosts     map[string]string            `yaml:"guest_hosts"`
	LockFreeTypes  []string                     `yaml:"lock_free_types"`
}

// Validate requires at least one host mapping.
// Validate rejects empty declared maps, keys, and host names.
// Validate requires guest keys in <node>/<vmid> form.
// Validate rejects a declared LockFreeTypes list with no types or an empty name.
func (t Targets) Validate() error {
	if t.ModuleHosts == nil && t.ModuleKeyHosts == nil && t.NodeHosts == nil && t.GuestHosts == nil {
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
	return nil
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
