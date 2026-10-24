package statemigration

import (
	"bytes"
	"encoding/json"
	"errors"
)

func canonicalJSON(data []byte) ([]byte, error) {
	value := bytes.TrimSpace(data)
	if !json.Valid(value) {
		return nil, errors.New("the private verification value is not valid JSON")
	}
	switch value[0] {
	case '{':
		return canonicalObject(value)
	case '[':
		return canonicalArray(value)
	case '"':
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return nil, errors.New("decode the private JSON string")
		}
		encoded, err := json.Marshal(text)
		if err != nil {
			return nil, errors.New("encode the private JSON string")
		}
		return encoded, nil
	default:
		return value, nil
	}
}

func canonicalObject(value []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(value, &fields); err != nil {
		return nil, errors.New("decode the private JSON object")
	}
	for key, contents := range fields {
		encoded, err := canonicalJSON(contents)
		if err != nil {
			return nil, err
		}
		fields[key] = encoded
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, errors.New("encode the private JSON object")
	}
	return encoded, nil
}

func canonicalArray(value []byte) ([]byte, error) {
	var elements []json.RawMessage
	if err := json.Unmarshal(value, &elements); err != nil {
		return nil, errors.New("decode the private JSON array")
	}
	for index, element := range elements {
		encoded, err := canonicalJSON(element)
		if err != nil {
			return nil, err
		}
		elements[index] = encoded
	}
	encoded, err := json.Marshal(elements)
	if err != nil {
		return nil, errors.New("encode the private JSON array")
	}
	return encoded, nil
}
