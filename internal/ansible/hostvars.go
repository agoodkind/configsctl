package ansible

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
)

// unsafeKey is the wrapper key of an untrusted string in ansible-inventory
// output. ansible-inventory writes an untrusted host variable, such as a
// Proxmox inventory compose result, as {"__ansible_unsafe": "<value>"}.
const unsafeKey = "__ansible_unsafe"

// UnmarshalJSON decodes each connection variable from a JSON string or from
// an untrusted-string wrapper object. An absent or null variable decodes to
// the empty string. Any other JSON type, and any other object, is an error.
func (v *HostVars) UnmarshalJSON(data []byte) error {
	var raw struct {
		AnsibleHost       json.RawMessage `json:"ansible_host"`
		AnsibleUser       json.RawMessage `json:"ansible_user"`
		AnsibleConnection json.RawMessage `json:"ansible_connection"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode host variables: %w", err)
	}
	fields := []struct {
		name  string
		raw   json.RawMessage
		value *string
	}{
		{name: "ansible_host", raw: raw.AnsibleHost, value: &v.AnsibleHost},
		{name: "ansible_user", raw: raw.AnsibleUser, value: &v.AnsibleUser},
		{name: "ansible_connection", raw: raw.AnsibleConnection, value: &v.AnsibleConnection},
	}
	for _, field := range fields {
		value, ok := inventoryString(field.raw)
		if !ok {
			return fmt.Errorf("decode %s: the value is neither a string nor an %s object", field.name, unsafeKey)
		}
		*field.value = value
	}
	return nil
}

// VariableHosts returns sorted, unique inventory host names from the requested
// variables on every supplied host.
func (inv Inventory) VariableHosts(hosts, variables []string) ([]string, error) {
	found := map[string]struct{}{}
	for _, host := range hosts {
		for _, variable := range variables {
			target, err := inv.variableHost(host, variable)
			if err != nil {
				return nil, err
			}
			found[target] = struct{}{}
		}
	}
	return sortedKeys(found), nil
}

func (inv Inventory) variableHost(host, variable string) (string, error) {
	raw, ok := inv.variables[host][variable]
	if !ok {
		return "", fmt.Errorf("host %s has no inventory variable %s", host, variable)
	}
	value, ok := inventoryString(raw)
	if !ok || value == "" {
		return "", fmt.Errorf("inventory variable %s of host %s is not a host name string", variable, host)
	}
	if !inv.hasHost(value) {
		return "", fmt.Errorf("inventory variable %s of host %s is %s, which is not an inventory host", variable, host, value)
	}
	return value, nil
}

// HostString returns a nonempty string from a host's inventory variable.
// The method returns an error for a missing variable, an empty string, or an invalid string value.
func (inv Inventory) HostString(host, variable string) (string, error) {
	raw, ok := inv.variables[host][variable]
	if !ok {
		return "", fmt.Errorf("host %s has no inventory variable %s", host, variable)
	}
	value, ok := inventoryString(raw)
	if !ok || value == "" {
		return "", fmt.Errorf("inventory variable %s of host %s is not a string", variable, host)
	}
	return value, nil
}

// HasVariable reports whether a host has the specified inventory variable.
func (inv Inventory) HasVariable(host, variable string) bool {
	_, ok := inv.variables[host][variable]
	return ok
}

// HostInteger returns a host's inventory variable as a nonnegative integer.
// It accepts a JSON integer, a JSON string of digits, or an untrusted-string
// wrapper object containing a string of digits.
func (inv Inventory) HostInteger(host, variable string) (int, error) {
	raw, ok := inv.variables[host][variable]
	if !ok {
		return 0, fmt.Errorf("host %s has no inventory variable %s", host, variable)
	}
	digits := string(raw)
	if !digitsPattern.MatchString(digits) {
		value, isString := inventoryString(raw)
		if !isString || !digitsPattern.MatchString(value) {
			return 0, fmt.Errorf("inventory variable %s of host %s is not an integer or a string of digits", variable, host)
		}
		digits = value
	}
	number, err := strconv.Atoi(digits)
	if err != nil {
		slog.Error("ansible.inventory.integer_failed", "host", host, "variable", variable, "err", err)
		return 0, fmt.Errorf("read inventory variable %s of host %s: %w", variable, host, err)
	}
	return number, nil
}

var digitsPattern = regexp.MustCompile(`^[0-9]+$`)

func (inv Inventory) hasHost(name string) bool {
	if _, ok := inv.Hosts[name]; ok {
		return true
	}
	for _, group := range inv.groups {
		if slices.Contains(group.Hosts, name) {
			return true
		}
	}
	return false
}

// inventoryString returns the string in raw. An absent or null value is the
// empty string.
func inventoryString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", true
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return plain, true
	}
	var wrapped map[string]*string
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return "", false
	}
	value, ok := wrapped[unsafeKey]
	if !ok || value == nil || len(wrapped) != 1 {
		return "", false
	}
	return *value, true
}
