package ansible

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// unsafeKey is the wrapper key of an untrusted string in ansible-inventory
// output. ansible-inventory writes an untrusted host variable, such as a
// Proxmox inventory compose result, as {"__ansible_unsafe": "<value>"}.
const unsafeKey = "__ansible_unsafe"

// UnmarshalJSON decodes each connection variable from a JSON string or from
// an untrusted-string wrapper object.
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
		value, err := inventoryString(field.raw)
		if err != nil {
			return fmt.Errorf("decode %s: %w", field.name, err)
		}
		*field.value = value
	}
	return nil
}

// inventoryString returns the string in raw. An absent or null value is the
// empty string. Any other JSON type, and any object other than the
// untrusted-string wrapper, is an error.
func inventoryString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return plain, nil
	}
	var wrapped map[string]string
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return "", fmt.Errorf("the value is not a string: %w", err)
	}
	value, ok := wrapped[unsafeKey]
	if !ok || len(wrapped) != 1 {
		return "", fmt.Errorf("the object value is not an %s string", unsafeKey)
	}
	return value, nil
}
