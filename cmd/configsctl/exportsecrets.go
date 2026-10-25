package main

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"goodkind.io/configsctl/internal/vault"
)

func runExportSecrets(_ cmdEnv, args []string) error {
	if len(args) < 3 || args[0] != "--directory" || args[1] == "" {
		return errors.New("usage: configsctl export-secrets --directory <NEW_PRIVATE_DIRECTORY> <source-key>[=<destination-key>] [<source-key>[=<destination-key>]]")
	}
	passwordFile, err := vaultPassPath()
	if err != nil {
		return err
	}
	names, err := vault.ExportSecrets(args[2:], args[1], defaultVaultFile, passwordFile)
	if err != nil {
		slog.Error("vault secret export failed", "directory", args[1], "err", err)
		return fmt.Errorf("export vault secrets: %w", err)
	}
	fmt.Printf("vault: %s\n", filepath.Join(args[1], "vault.yml"))
	fmt.Printf("password file: %s\n", filepath.Join(args[1], "vault.pass"))
	fmt.Printf("exported %d keys: %s\n", len(names), strings.Join(names, ", "))
	return nil
}
