package tofusecrets

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"goodkind.io/configsctl/internal/vault"
)

const tofuVariablePrefix = "TF_VAR_"

// ReadAssignments returns the secrets that OpenTofu can read.
//
// Environment mode passes the configured variables. The legacy mode exports
// vault keys that match declared variables or start with the configured prefix.
//
//	assignments, err := inputs.ReadAssignments(names, prefix, file, passwordFile)
func (input *Environment) ReadAssignments(
	names []string,
	prefix string,
	file string,
	passwordFile string,
) ([]string, error) {
	if input != nil {
		return input.assignments(), nil
	}

	values, err := vault.Values(file, passwordFile)
	if err != nil {
		slog.Error("Read the vault for OpenTofu variables", "err", err)

		return nil, fmt.Errorf("read vault for tofu variables: %w", err)
	}

	var assignments []string

	exportedAsVariable := make(map[string]bool, len(names))
	for _, name := range names {
		value, ok := values[name]
		if !ok {
			continue
		}

		assignments = append(assignments, tofuVariablePrefix+name+"="+value)
		exportedAsVariable[name] = true
	}

	var envKeys []string

	for key := range values {
		// A declared input already exported this vault key.
		if exportedAsVariable[key] {
			continue
		}

		if name, ok := strings.CutPrefix(key, prefix); ok && name != "" {
			envKeys = append(envKeys, key)
		}
	}

	sort.Strings(envKeys)

	for _, key := range envKeys {
		assignments = append(assignments, strings.TrimPrefix(key, prefix)+"="+values[key])
	}

	return assignments, nil
}
