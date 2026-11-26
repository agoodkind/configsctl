package vault

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

const tofuVaultVariable = "CONFIGSCTL_VAULT_FILE"

var (
	errRuntimeAbsolutePath   = errors.New("CONFIGSCTL_VAULT_FILE must be a clean absolute path")
	errRuntimeSymlinkPath    = errors.New("CONFIGSCTL_VAULT_FILE must not use symbolic links")
	errRuntimeInsideCheckout = errors.New("CONFIGSCTL_VAULT_FILE must be outside the checkout")
	errRuntimeUnsafeMode     = errors.New("CONFIGSCTL_VAULT_FILE must be a mode-0600 regular file")
)

// RuntimeFile selects the vault for a Git-backed OpenTofu run.
//
// The default is the vault in the Configs checkout. An explicit selection
// must identify a private regular file outside that checkout.
//
// A job can select an encrypted vault before it runs configsctl:
//
//	CONFIGSCTL_VAULT_FILE=/run/creds/vault.yml
func RuntimeFile() (string, error) {
	path, configured := os.LookupEnv(tofuVaultVariable)
	if !configured {
		return File, nil
	}

	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errRuntimeAbsolutePath
	}

	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		slog.Error("Could not resolve CONFIGSCTL_VAULT_FILE", "err", err)

		return "", fmt.Errorf("resolve CONFIGSCTL_VAULT_FILE: %w", err)
	}

	if canonical != path {
		return "", errRuntimeSymlinkPath
	}

	checkout, err := os.Getwd()
	if err != nil {
		slog.Error("Could not find the OpenTofu checkout", "err", err)

		return "", fmt.Errorf("find the OpenTofu checkout: %w", err)
	}

	checkout, err = filepath.EvalSymlinks(checkout)
	if err != nil {
		slog.Error("Could not resolve the OpenTofu checkout", "err", err)

		return "", fmt.Errorf("resolve the OpenTofu checkout: %w", err)
	}

	relative, err := filepath.Rel(checkout, path)
	if err != nil || filepath.IsLocal(relative) {
		return "", errRuntimeInsideCheckout
	}

	info, err := os.Stat(path)
	if err != nil {
		slog.Error("Could not inspect CONFIGSCTL_VAULT_FILE", "err", err)

		return "", fmt.Errorf("inspect CONFIGSCTL_VAULT_FILE: %w", err)
	}

	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return "", errRuntimeUnsafeMode
	}

	return path, nil
}
