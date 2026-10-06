package main

import (
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

func savedPlanCandidate(args []string) string {
	if len(args) < 2 || tofuSubcommand(args[:len(args)-1]) != tofuApply {
		return ""
	}
	candidate := args[len(args)-1]
	if strings.HasPrefix(candidate, "-") || consumesTofuValue(args[len(args)-2]) {
		return ""
	}
	return candidate
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
