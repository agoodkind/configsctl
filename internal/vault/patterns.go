package vault

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"goodkind.io/configsctl/internal/redact"
)

// File is the vault path relative to the repository root.
const File = "ansible/inventory/group_vars/all/vault.yml"

// Patterns decrypts the vault under root with passwordFile and returns one
// redaction pattern for each non-empty value, labeled with its key.
func Patterns(root, passwordFile string) ([]redact.Pattern, error) {
	vaultPath := filepath.Join(root, File)
	values, err := Values(vaultPath, passwordFile)
	if err != nil {
		slog.Error("vault patterns load failed", "vault", vaultPath, "err", err)
		return nil, fmt.Errorf("load vault values from %s: %w", vaultPath, err)
	}
	patterns := make([]redact.Pattern, 0, len(values))
	for name, value := range values {
		if value != "" {
			patterns = append(patterns, redact.Pattern{Value: []byte(value), Label: name})
		}
	}
	return patterns, nil
}
