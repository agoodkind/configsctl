package vault

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const exportPasswordBytes = 32

var exportKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ExportSecrets creates a separate encrypted vault with only the selected keys.
// A selection uses source-key or source-key=destination-key syntax.
func ExportSecrets(selections []string, directory, vaultPath, passwordFile string) (names []string, exportErr error) {
	selected, err := exportMapping(selections, vaultPath, passwordFile)
	if err != nil {
		return nil, err
	}
	passwordBytes := make([]byte, exportPasswordBytes)
	if _, err := rand.Read(passwordBytes); err != nil {
		slog.Error("export password generation failed", "err", err)
		return nil, fmt.Errorf("generate export password: %w", err)
	}
	password := hex.EncodeToString(passwordBytes)
	if err := os.Mkdir(directory, 0o700); err != nil {
		slog.Error("export directory creation failed", "directory", directory, "err", err)
		return nil, fmt.Errorf("create new export directory %q: %w", directory, err)
	}
	created := make([]string, 0, 2)
	defer func() {
		if exportErr != nil {
			exportErr = errors.Join(exportErr, removeExport(directory, created))
		}
	}()
	if err := os.Chmod(directory, 0o700); err != nil {
		slog.Error("export directory permission change failed", "directory", directory, "err", err)
		return nil, fmt.Errorf("set export directory permissions: %w", err)
	}
	passwordPath := filepath.Join(directory, "vault.pass")
	createdPassword, err := createExportFile(passwordPath, password+"\n")
	if createdPassword {
		created = append(created, passwordPath)
	}
	if err != nil {
		return nil, err
	}
	exportedVault := filepath.Join(directory, "vault.yml")
	createdVault, err := createExportFile(exportedVault, "")
	if createdVault {
		created = append(created, exportedVault)
	}
	if err != nil {
		return nil, err
	}
	if err := encryptMapping(exportedVault, password, selected); err != nil {
		return nil, err
	}
	names = make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func exportMapping(selections []string, vaultPath, passwordFile string) (map[string]string, error) {
	if len(selections) == 0 {
		return nil, errors.New("select at least one vault key")
	}
	sources := make(map[string]string, len(selections))
	for _, selection := range selections {
		source, destination, remapped := strings.Cut(selection, "=")
		if !remapped {
			destination = source
		}
		if !exportKeyPattern.MatchString(source) || !exportKeyPattern.MatchString(destination) {
			return nil, fmt.Errorf("invalid key selection %q: use identifiers matching [A-Za-z_][A-Za-z0-9_]*", selection)
		}
		if _, exists := sources[destination]; exists {
			return nil, fmt.Errorf("duplicate destination key: %s", destination)
		}
		sources[destination] = source
	}
	existing, err := decryptMapping(vaultPath, passwordFile)
	if err != nil {
		return nil, err
	}
	selected := make(map[string]string, len(sources))
	for destination, source := range sources {
		value, exists := existing[source]
		if !exists {
			return nil, fmt.Errorf("vault key not found: %s", source)
		}
		selected[destination] = value
	}
	return selected, nil
}

func createExportFile(path, content string) (bool, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		slog.Error("export file creation failed", "path", path, "err", err)
		return false, fmt.Errorf("create export file %q: %w", path, err)
	}
	permissionErr := file.Chmod(0o600)
	var writeErr error
	if permissionErr == nil {
		_, writeErr = file.WriteString(content)
	}
	if err := errors.Join(permissionErr, writeErr, file.Close()); err != nil {
		slog.Error("export file write failed", "path", path, "err", err)
		return true, fmt.Errorf("write export file %q: %w", path, err)
	}
	return true, nil
}

func removeExport(directory string, created []string) error {
	var cleanupErr error
	for _, path := range created {
		if err := os.Remove(path); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove failed export file %q: %w", path, err))
		}
	}
	if err := os.Remove(directory); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove failed export directory %q: %w", directory, err))
	}
	if cleanupErr != nil {
		slog.Error("failed export cleanup failed", "directory", directory, "err", cleanupErr)
	}
	return cleanupErr
}
