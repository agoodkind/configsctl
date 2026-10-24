package statemigration

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
)

func changedAttributePaths(before, after, unknown json.RawMessage) ([]string, error) {
	paths, err := attributeDifferences(before, after, "")
	if err != nil {
		return nil, err
	}
	unknownPaths, err := unknownAttributePaths(unknown, "")
	if err != nil {
		return nil, err
	}
	paths = append(paths, unknownPaths...)
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

func attributeDifferences(before, after json.RawMessage, path string) ([]string, error) {
	left, err := canonicalJSON(attributeValue(before))
	if err != nil {
		return nil, err
	}
	right, err := canonicalJSON(attributeValue(after))
	if err != nil {
		return nil, err
	}
	if bytes.Equal(left, right) {
		return nil, nil
	}
	if left[0] == '{' || right[0] == '{' {
		return objectDifferences(left, right, path)
	}
	if left[0] == '[' || right[0] == '[' {
		return arrayDifferences(left, right, path)
	}
	return []string{path}, nil
}

func attributeValue(value json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(value)) == 0 {
		return json.RawMessage("null")
	}
	return bytes.TrimSpace(value)
}

func objectAttributes(value json.RawMessage) (map[string]json.RawMessage, error) {
	if attributeValue(value)[0] != '{' {
		return nil, nil
	}
	var attributes map[string]json.RawMessage
	if err := json.Unmarshal(value, &attributes); err != nil {
		return nil, errors.New("decode private resource object attributes")
	}
	return attributes, nil
}

func objectDifferences(before, after json.RawMessage, path string) ([]string, error) {
	left, err := objectAttributes(before)
	if err != nil {
		return nil, err
	}
	right, err := objectAttributes(after)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]bool, len(left)+len(right))
	for key := range left {
		keys[key] = true
	}
	for key := range right {
		keys[key] = true
	}
	var paths []string
	for key := range keys {
		changed, err := attributeDifferences(left[key], right[key], attributeKeyPath(path, key))
		if err != nil {
			return nil, err
		}
		paths = append(paths, changed...)
	}
	if len(paths) == 0 {
		return []string{path}, nil
	}
	return paths, nil
}

func arrayAttributes(value json.RawMessage) ([]json.RawMessage, error) {
	if attributeValue(value)[0] != '[' {
		return nil, nil
	}
	var attributes []json.RawMessage
	if err := json.Unmarshal(value, &attributes); err != nil {
		return nil, errors.New("decode private resource array attributes")
	}
	return attributes, nil
}

func arrayDifferences(before, after json.RawMessage, path string) ([]string, error) {
	left, err := arrayAttributes(before)
	if err != nil {
		return nil, err
	}
	right, err := arrayAttributes(after)
	if err != nil {
		return nil, err
	}
	var paths []string
	for index := range max(len(left), len(right)) {
		var oldValue, newValue json.RawMessage
		if index < len(left) {
			oldValue = left[index]
		}
		if index < len(right) {
			newValue = right[index]
		}
		changed, err := attributeDifferences(oldValue, newValue, path+"/"+strconv.Itoa(index))
		if err != nil {
			return nil, err
		}
		paths = append(paths, changed...)
	}
	if len(paths) == 0 {
		return []string{path}, nil
	}
	return paths, nil
}

func unknownAttributePaths(value json.RawMessage, path string) ([]string, error) {
	value = attributeValue(value)
	switch value[0] {
	case '{':
		attributes, err := objectAttributes(value)
		if err != nil {
			return nil, err
		}
		var paths []string
		for key, item := range attributes {
			unknown, err := unknownAttributePaths(item, attributeKeyPath(path, key))
			if err != nil {
				return nil, err
			}
			paths = append(paths, unknown...)
		}
		return paths, nil
	case '[':
		return unknownArrayPaths(value, path)
	case 't':
		return []string{path}, nil
	}
	return nil, nil
}

func unknownArrayPaths(value json.RawMessage, path string) ([]string, error) {
	attributes, err := arrayAttributes(value)
	if err != nil {
		return nil, err
	}
	var paths []string
	for index, item := range attributes {
		unknown, err := unknownAttributePaths(item, path+"/"+strconv.Itoa(index))
		if err != nil {
			return nil, err
		}
		paths = append(paths, unknown...)
	}
	return paths, nil
}

func attributeKeyPath(path, key string) string {
	if path == "" {
		escaped := strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
		return "/" + escaped
	}
	// Nested object keys can contain user values, including sensitive map keys.
	return path + "/key-sha256:" + digest([]byte(key))
}
