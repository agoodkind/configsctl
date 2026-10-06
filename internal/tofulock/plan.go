package tofulock

import (
	"bytes"
	"encoding/json"
	"slices"
)

const (
	nodeAttribute = "node"
	vmidAttribute = "vmid"
	deleteAction  = "delete"
)

var (
	jsonNull  = []byte("null")
	jsonTrue  = []byte("true")
	jsonFalse = []byte("false")
)

var inertActions = map[string]bool{"no-op": true, "read": true}

type planDocument struct {
	FormatVersion   string           `json:"format_version"`
	ResourceChanges []resourceChange `json:"resource_changes"`
}

type resourceChange struct {
	Address string       `json:"address"`
	Type    string       `json:"type"`
	Change  changeValues `json:"change"`
}

type changeValues struct {
	Actions      []string                   `json:"actions"`
	Before       map[string]json.RawMessage `json:"before"`
	After        map[string]json.RawMessage `json:"after"`
	AfterUnknown json.RawMessage            `json:"after_unknown"`
}

type resourceState struct {
	attributes map[string]json.RawMessage
	unknown    map[string]bool
	allUnknown bool
}

type changeStates struct {
	before *resourceState
	after  *resourceState
}

func (c changeValues) inert() bool {
	if len(c.Actions) == 0 {
		return false
	}
	for _, action := range c.Actions {
		if !inertActions[action] {
			return false
		}
	}
	return true
}

func (c changeValues) deletes() bool {
	return slices.Contains(c.Actions, deleteAction)
}

func (c changeValues) states() changeStates {
	var states changeStates
	unknown, allUnknown := unknownAttributes(c.AfterUnknown)
	if c.Before != nil {
		states.before = &resourceState{attributes: c.Before, unknown: nil, allUnknown: false}
	}
	if c.After != nil || allUnknown {
		states.after = &resourceState{attributes: c.After, unknown: unknown, allUnknown: allUnknown}
	}
	return states
}

func (s changeStates) present() []*resourceState {
	var present []*resourceState
	for _, state := range []*resourceState{s.before, s.after} {
		if state != nil {
			present = append(present, state)
		}
	}
	return present
}

func (s changeStates) nodeUnknown() bool {
	return s.after != nil && s.after.isUnknown(nodeAttribute)
}

func unknownAttributes(raw json.RawMessage) (map[string]bool, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, jsonFalse) || bytes.Equal(trimmed, jsonNull) {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return nil, true
	}
	unknown := map[string]bool{}
	for name, value := range object {
		unknown[name] = bytes.Equal(bytes.TrimSpace(value), jsonTrue)
	}
	return unknown, false
}

func (s *resourceState) isUnknown(name string) bool {
	return s.allUnknown || s.unknown[name]
}

func (s *resourceState) text(name string) (string, bool) {
	raw, ok := s.attributes[name]
	if !ok || s.isUnknown(name) {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return value, value != ""
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String(), true
	}
	return "", false
}
