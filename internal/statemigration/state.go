// Package statemigration transfers existing state records with native OpenTofu commands.
package statemigration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
)

// StateInfo contains verification metadata without resource attribute values.
type StateInfo struct {
	Lineage      string              `json:"lineage"`
	Serial       uint64              `json:"serial"`
	Records      map[string]string   `json:"records"`
	Digest       string              `json:"digest"`
	Content      string              `json:"content_digest"`
	Dependencies map[string][]string `json:"dependencies"`
}

// EmptyState represents an initialized backend without a state file.
func EmptyState() StateInfo {
	return StateInfo{Lineage: "", Serial: 0, Records: nil, Digest: "", Content: "", Dependencies: nil}
}

type stateDocument struct {
	Version   int             `json:"version"`
	Lineage   string          `json:"lineage"`
	Serial    uint64          `json:"serial"`
	Resources []stateResource `json:"resources"`
}

type stateResource struct {
	Module    string                       `json:"module"`
	Mode      string                       `json:"mode"`
	Type      string                       `json:"type"`
	Name      string                       `json:"name"`
	Provider  string                       `json:"provider"`
	Instances []map[string]json.RawMessage `json:"instances"`
}

// Inspect reads a private native state export and returns only its verification metadata.
func Inspect(path string) (StateInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("migration.state.read_failed", "path", path, "err", migrationErrorType(err))
		return EmptyState(), fmt.Errorf("read private state export: %w", err)
	}
	var state stateDocument
	if err := json.Unmarshal(data, &state); err != nil {
		return StateInfo{}, errors.New("the private state export is not valid JSON")
	}
	if state.Version != 4 || state.Lineage == "" {
		return StateInfo{}, errors.New("the private state export must use state format 4 and include a lineage")
	}
	info := EmptyState()
	info.Lineage, info.Serial = state.Lineage, state.Serial
	info.Records = make(map[string]string)
	info.Dependencies = make(map[string][]string)
	for _, resource := range state.Resources {
		for _, instance := range resource.Instances {
			address, err := instanceAddress(resource, instance)
			if err != nil {
				return StateInfo{}, err
			}
			if _, exists := info.Records[address]; exists {
				return StateInfo{}, fmt.Errorf("the state contains duplicate instance address %s", address)
			}
			proof, err := recordDigest(resource, instance)
			if err != nil {
				return StateInfo{}, err
			}
			info.Records[address] = proof
			if dependencies, exists := instance["dependencies"]; exists {
				var values []string
				if err := json.Unmarshal(dependencies, &values); err != nil {
					return StateInfo{}, fmt.Errorf("instance %s contains invalid dependency metadata", address)
				}
				info.Dependencies[address] = values
			}
		}
	}
	canonical, err := json.Marshal(info.Records)
	if err != nil {
		return StateInfo{}, fmt.Errorf("encode state verification metadata: %w", err)
	}
	info.Digest = digest(canonical)
	content, err := contentDigest(data)
	if err != nil {
		return StateInfo{}, err
	}
	info.Content = content
	return info, nil
}

func contentDigest(data []byte) (string, error) {
	var content map[string]json.RawMessage
	if err := json.Unmarshal(data, &content); err != nil {
		return "", errors.New("decode private state verification data")
	}
	delete(content, "serial")
	delete(content, "lineage")
	delete(content, "terraform_version")
	if err := normalizeStateCheckResults(content); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return "", errors.New("encode private state verification data")
	}
	canonical, err := canonicalJSON(encoded)
	if err != nil {
		return "", err
	}
	return digest(canonical), nil
}

func instanceAddress(resource stateResource, instance map[string]json.RawMessage) (string, error) {
	address := resource.Type + "." + resource.Name
	if resource.Mode == "data" {
		address = "data." + address
	} else if resource.Mode != "managed" {
		return "", fmt.Errorf("resource %s has an unsupported state mode", address)
	}
	if resource.Module != "" {
		address = resource.Module + "." + address
	}
	if deposed, exists := instance["deposed"]; exists && string(deposed) != `""` {
		return "", fmt.Errorf("resource %s has a deposed instance; resolve its interrupted replacement before migration", address)
	}
	if key, exists := instance["index_key"]; exists && string(key) != "null" {
		index, err := instanceIndex(key)
		if err != nil {
			return "", fmt.Errorf("resource %s has an invalid instance key", address)
		}
		address += "[" + index + "]"
	}
	return address, nil
}

func recordDigest(resource stateResource, instance map[string]json.RawMessage) (string, error) {
	// Native moves change index keys and invalidate dependency lists.
	contents := make(map[string]json.RawMessage, len(instance))
	for key, value := range instance {
		if key != "index_key" && key != "dependencies" {
			contents[key] = value
		}
	}
	encoded, err := json.Marshal(struct {
		Provider string                     `json:"provider"`
		Type     string                     `json:"type"`
		Mode     string                     `json:"mode"`
		Instance map[string]json.RawMessage `json:"instance"`
	}{resource.Provider, resource.Type, resource.Mode, contents})
	if err != nil {
		return "", errors.New("encode private resource verification data")
	}
	canonical, err := canonicalJSON(encoded)
	if err != nil {
		return "", errors.New("encode normalized private resource verification data")
	}
	return digest(canonical), nil
}

func instanceIndex(raw json.RawMessage) (string, error) {
	value := bytes.TrimSpace(raw)
	if len(value) != 0 && value[0] == '"' {
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return "", errors.New("decode the native string instance key")
		}
		encoded, err := json.Marshal(text)
		if err != nil {
			return "", errors.New("encode the native string instance key")
		}
		return string(encoded), nil
	}
	index, err := strconv.ParseUint(string(value), 10, 64)
	if err != nil || strconv.FormatUint(index, 10) != string(value) {
		return "", errors.New("decode the native numeric instance key")
	}
	return string(value), nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Addresses returns sorted instance addresses without their attribute values.
func (s StateInfo) Addresses() []string {
	addresses := make([]string, 0, len(s.Records))
	for address := range s.Records {
		addresses = append(addresses, address)
	}
	slices.Sort(addresses)
	return addresses
}
