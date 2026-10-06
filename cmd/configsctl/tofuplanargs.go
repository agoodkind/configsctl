package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Boolean flags from tofu apply -help and tofu plan -help may precede a saved
// plan without consuming its path.
var tofuBooleanFlags = map[string]bool{
	"auto-approve":           true,
	"compact-warnings":       true,
	"concise":                true,
	"consolidate-errors":     true,
	"consolidate-warnings":   true,
	"destroy":                true,
	"detailed-exitcode":      true,
	"input":                  true,
	"json":                   true,
	"lock":                   true,
	"no-color":               true,
	"refresh":                true,
	"refresh-only":           true,
	"show-sensitive":         true,
	"suppress-forget-errors": true,
}

func savedPlanFile(workspaceDir string, args []string) (string, error) {
	if len(args) < 2 || tofuSubcommand(args[:len(args)-1]) != tofuApply {
		return "", nil
	}
	candidate := args[len(args)-1]
	if strings.HasPrefix(candidate, "-") || consumesTofuValue(args[len(args)-2]) {
		return "", nil
	}
	path := workspacePath(workspaceDir, candidate)
	info, err := os.Stat(path)
	if err != nil {
		slog.Error("tofu.lock.plan_stat_failed", "plan", path, "err", err)
		return "", fmt.Errorf("read the saved plan %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		err := fmt.Errorf("the saved plan %s is not a regular file", path)
		slog.Error("tofu.lock.plan_not_regular", "plan", path, "err", err)
		return "", err
	}
	return candidate, nil
}

func consumesTofuValue(arg string) bool {
	if !strings.HasPrefix(arg, "-") || strings.Contains(arg, "=") {
		return false
	}
	return !tofuBooleanFlags[strings.TrimLeft(arg, "-")]
}

func workspacePath(workspaceDir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(workspaceDir, path)
}
