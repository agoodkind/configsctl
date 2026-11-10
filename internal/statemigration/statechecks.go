package statemigration

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
)

// OpenTofu serializes check groups and checked objects from address maps.
func normalizeStateCheckResults(content map[string]json.RawMessage) error {
	raw, exists := content["check_results"]
	if !exists {
		return nil
	}
	var results []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &results); err != nil {
		return errors.New("decode private native check-result groups")
	}
	for _, result := range results {
		objects, exists := result["objects"]
		if !exists {
			continue
		}
		normalized, err := canonicalUnorderedStateArray(objects)
		if err != nil {
			return err
		}
		result["objects"] = normalized
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		return errors.New("encode private native check-result groups")
	}
	normalized, err := canonicalUnorderedStateArray(encoded)
	if err != nil {
		return err
	}
	content["check_results"] = normalized
	return nil
}

func canonicalUnorderedStateArray(raw []byte) ([]byte, error) {
	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil {
		return nil, errors.New("decode a private unordered native state collection")
	}
	for index, element := range elements {
		encoded, err := canonicalJSON(element)
		if err != nil {
			return nil, err
		}
		elements[index] = encoded
	}
	slices.SortFunc(elements, func(left, right json.RawMessage) int { return bytes.Compare(left, right) })
	encoded, err := json.Marshal(elements)
	if err != nil {
		return nil, errors.New("encode a private unordered native state collection")
	}
	return encoded, nil
}
