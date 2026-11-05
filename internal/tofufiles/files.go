// Package tofufiles selects the OpenTofu source files that a module evaluates.
package tofufiles

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Discover returns active native and JSON source files in directory order.
// A .tofu file replaces the matching .tf file with the same encoding.
func Discover(directory string) (native, jsonFiles []string, err error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		slog.Error("tofu.source.discovery_failed", "directory", directory, "err", err)
		return nil, nil, fmt.Errorf("read OpenTofu source directory %s: %w", directory, err)
	}
	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names[entry.Name()] = true
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || ignored(name) || !sourceFile(name) || replaced(name, names) {
			continue
		}
		path := filepath.Join(directory, name)
		if strings.HasSuffix(name, ".json") {
			jsonFiles = append(jsonFiles, path)
		} else {
			native = append(native, path)
		}
	}
	return native, jsonFiles, nil
}

func ignored(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") ||
		(strings.HasPrefix(name, "#") && strings.HasSuffix(name, "#"))
}

func sourceFile(name string) bool {
	return strings.HasSuffix(name, ".tf") || strings.HasSuffix(name, ".tf.json") ||
		strings.HasSuffix(name, ".tofu") || strings.HasSuffix(name, ".tofu.json")
}

func replaced(name string, names map[string]bool) bool {
	if stem, matched := strings.CutSuffix(name, ".tf"); matched {
		return names[stem+".tofu"]
	}
	if stem, matched := strings.CutSuffix(name, ".tf.json"); matched {
		return names[stem+".tofu.json"]
	}
	return false
}
