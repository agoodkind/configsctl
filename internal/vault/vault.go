// Package vault reads and updates the Ansible vault directly in the AES256 vault
// format, so secret access does not shell out to ansible-vault. The vault is a
// flat mapping of name to secret string.
package vault

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"sort"
	"strings"

	ansiblevault "github.com/sosedoff/ansible-vault-go"
	"gopkg.in/yaml.v3"
)

// Keys returns the vault key names, sorted.
func Keys(vaultPath, passwordFile string) ([]string, error) {
	mapping, err := decryptMapping(vaultPath, passwordFile)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(mapping))
	for name := range mapping {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Secret returns one vault value.
func Secret(key, vaultPath, passwordFile string) (string, error) {
	mapping, err := decryptMapping(vaultPath, passwordFile)
	if err != nil {
		return "", err
	}
	value, ok := mapping[key]
	if !ok {
		return "", fmt.Errorf("vault key not found: %s", key)
	}
	return value, nil
}

// SetSecrets merges a YAML mapping into the vault, preserving other keys, and
// reports the added and updated key names.
func SetSecrets(stdin, vaultPath, passwordFile string) ([]string, []string, error) {
	incoming, err := parseMapping(stdin)
	if err != nil {
		return nil, nil, err
	}
	if len(incoming) == 0 {
		return nil, nil, errors.New("stdin must contain a YAML mapping of key to value")
	}
	password, err := readPassword(passwordFile)
	if err != nil {
		return nil, nil, err
	}
	existing, err := decryptMapping(vaultPath, passwordFile)
	if err != nil {
		return nil, nil, err
	}
	added, updated := classifyKeys(incoming, existing)
	merged := make(map[string]string, len(existing)+len(incoming))
	maps.Copy(merged, existing)
	maps.Copy(merged, incoming)
	if err := encryptMapping(vaultPath, password, merged); err != nil {
		return nil, nil, err
	}
	return added, updated, nil
}

func decryptMapping(vaultPath, passwordFile string) (map[string]string, error) {
	password, err := readPassword(passwordFile)
	if err != nil {
		return nil, err
	}
	plain, err := ansiblevault.DecryptFile(vaultPath, password)
	if err != nil {
		slog.Error("vault decrypt failed", "path", vaultPath, "err", err)
		return nil, fmt.Errorf("decrypt %s: %w", vaultPath, err)
	}
	return parseMapping(plain)
}

func encryptMapping(vaultPath, password string, mapping map[string]string) error {
	out, err := yaml.Marshal(mapping)
	if err != nil {
		slog.Error("vault marshal failed", "err", err)
		return fmt.Errorf("marshal vault: %w", err)
	}
	if err := ansiblevault.EncryptFile(vaultPath, string(out), password); err != nil {
		slog.Error("vault encrypt failed", "path", vaultPath, "err", err)
		return fmt.Errorf("encrypt %s: %w", vaultPath, err)
	}
	return nil
}

func parseMapping(content string) (map[string]string, error) {
	if strings.TrimSpace(content) == "" {
		return map[string]string{}, nil
	}
	mapping := map[string]string{}
	if err := yaml.Unmarshal([]byte(content), &mapping); err != nil {
		slog.Error("vault yaml parse failed", "err", err)
		return nil, fmt.Errorf("parse vault yaml: %w", err)
	}
	return mapping, nil
}

func readPassword(passwordFile string) (string, error) {
	data, err := os.ReadFile(passwordFile)
	if err != nil {
		slog.Error("vault password read failed", "path", passwordFile, "err", err)
		return "", fmt.Errorf("read vault password: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func classifyKeys(incoming, existing map[string]string) ([]string, []string) {
	var added, updated []string
	for key := range incoming {
		if _, ok := existing[key]; ok {
			updated = append(updated, key)
		} else {
			added = append(added, key)
		}
	}
	sort.Strings(added)
	sort.Strings(updated)
	return added, updated
}
