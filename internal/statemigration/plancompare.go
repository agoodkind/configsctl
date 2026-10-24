package statemigration

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

type planEntryProof struct {
	Kind      string `json:"kind"`
	Workspace string `json:"workspace"`
	Address   string `json:"address"`
	Digest    string `json:"digest"`
}

type planBaseline struct {
	Version  int              `json:"version"`
	Original []planEntryProof `json:"original"`
	Cutover  []planEntryProof `json:"cutover"`
	Forward  bool             `json:"forward_verified"`
}

type planResourceKey struct {
	workspace string
	address   string
}

func (e Engine) planMappings(original bool) map[planResourceKey]planResourceKey {
	mapping := make(map[planResourceKey]planResourceKey)
	for _, transfer := range e.Manifest.Moves {
		destination := planResourceKey{workspace: transfer.Destination, address: transfer.NewAddress}
		key := destination
		if original {
			key = planResourceKey{workspace: transfer.Source, address: transfer.Address}
		}
		mapping[key] = destination
	}
	return mapping
}

func (e Engine) planProof(contents []byte, selector string, original bool) ([]planEntryProof, error) {
	var document struct {
		Format  string            `json:"format_version"`
		Errored bool              `json:"errored"`
		Changes []json.RawMessage `json:"resource_changes"`
		Drift   []json.RawMessage `json:"resource_drift"`
	}
	if err := json.Unmarshal(contents, &document); err != nil || document.Format != "1.2" || document.Errored {
		return nil, errors.New("baseline comparison requires a successful supported native plan document")
	}
	proof := []planEntryProof{}
	mapping := e.planMappings(original)
	for _, group := range []struct {
		kind    string
		entries []json.RawMessage
	}{{"change", document.Changes}, {"drift", document.Drift}} {
		seen := make(map[planResourceKey]bool)
		for _, raw := range group.entries {
			entry, managed, err := normalizePlanEntry(raw, selector, group.kind, mapping)
			if err != nil {
				return nil, err
			}
			if !managed {
				continue
			}
			key := planResourceKey{workspace: entry.Workspace, address: entry.Address}
			if seen[key] {
				return nil, fmt.Errorf("the native %s plan repeats managed identity %s in %s", group.kind, entry.Address, entry.Workspace)
			}
			seen[key] = true
			proof = append(proof, entry)
		}
	}
	sortPlanProof(proof)
	return proof, nil
}

func normalizePlanEntry(raw json.RawMessage, selector, kind string, mapping map[planResourceKey]planResourceKey) (planEntryProof, bool, error) {
	var empty planEntryProof
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return empty, false, errors.New("decode private native plan entry")
	}
	var mode, address, deposed string
	if json.Unmarshal(fields["mode"], &mode) != nil || json.Unmarshal(fields["address"], &address) != nil {
		return empty, false, errors.New("the native plan entry omits its instance identity")
	}
	if mode == "data" {
		return empty, false, nil
	}
	if mode != "managed" {
		return empty, false, errors.New("the native plan entry has an unsupported instance mode")
	}
	if raw, exists := fields["deposed"]; exists && (json.Unmarshal(raw, &deposed) != nil || deposed != "") {
		return empty, false, errors.New("baseline comparison refuses deposed managed instances")
	}
	normalized, err := resolvePlanEntryMapping(fields, selector, address, mapping)
	if err != nil {
		return empty, false, err
	}
	if err := normalizePlanIdentity(fields, normalized.address); err != nil {
		return empty, false, err
	}
	if err := normalizePreviousPlanAddress(fields, selector, mapping); err != nil {
		return empty, false, err
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return empty, false, errors.New("encode complete private plan entry")
	}
	canonical, err := canonicalJSON(encoded)
	if err != nil {
		return empty, false, err
	}
	return planEntryProof{Kind: kind, Workspace: normalized.workspace, Address: normalized.address, Digest: digest(canonical)}, true, nil
}

func resolvePlanEntryMapping(fields map[string]json.RawMessage, selector, address string, mapping map[planResourceKey]planResourceKey) (planResourceKey, error) {
	key := planResourceKey{workspace: selector, address: address}
	if raw, moved := fields["previous_address"]; moved {
		var previous string
		if json.Unmarshal(raw, &previous) != nil {
			return planResourceKey{workspace: "", address: ""}, errors.New("the native plan contains an invalid previous instance address")
		}
		key.address = previous
	}
	normalized, known := mapping[key]
	if !known {
		return normalized, fmt.Errorf("the native plan contains unmapped managed instance %s in %s; pending creates cannot acquire migration ownership", address, selector)
	}
	if _, moved := fields["previous_address"]; moved && normalized.address != address {
		return normalized, errors.New("the pending native move target differs from its reviewed migration destination")
	}
	return normalized, nil
}

func normalizePreviousPlanAddress(fields map[string]json.RawMessage, selector string, mapping map[planResourceKey]planResourceKey) error {
	raw, exists := fields["previous_address"]
	if !exists {
		return nil
	}
	var address string
	if json.Unmarshal(raw, &address) != nil {
		return errors.New("the native plan contains an invalid previous instance address")
	}
	normalized, known := mapping[planResourceKey{workspace: selector, address: address}]
	if !known {
		return errors.New("the native plan contains an unmapped previous instance address")
	}
	var current string
	if json.Unmarshal(fields["address"], &current) != nil {
		return errors.New("the native plan contains an invalid current instance address")
	}
	if normalized.address == current {
		delete(fields, "previous_address")
		return nil
	}
	encoded, err := json.Marshal(normalized.address)
	if err != nil {
		return errors.New("encode the normalized previous instance address")
	}
	fields["previous_address"] = encoded
	return nil
}

func sortPlanProof(proof []planEntryProof) {
	slices.SortFunc(proof, func(left, right planEntryProof) int {
		if order := cmp.Compare(left.Kind, right.Kind); order != 0 {
			return order
		}
		if order := cmp.Compare(left.Workspace, right.Workspace); order != 0 {
			return order
		}
		return cmp.Compare(left.Address, right.Address)
	})
}

func comparePlanProof(original, cutover []planEntryProof) error {
	if slices.Equal(original, cutover) {
		return nil
	}
	differences := planDifferences(original, cutover)
	if len(differences) == 0 {
		return errors.New("the recorded native plan baseline contains duplicate or incorrectly ordered identity metadata")
	}
	return &planComparisonError{Differences: differences}
}
