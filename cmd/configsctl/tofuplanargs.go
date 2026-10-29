package main

import (
	"path/filepath"
	"strings"

	"goodkind.io/configsctl/internal/tofuargs"
)

func savedPlanCandidate(args []string) string {
	if len(args) < 2 || tofuargs.Subcommand(args[:len(args)-1]) != tofuargs.Apply {
		return ""
	}
	candidate := args[len(args)-1]
	if strings.HasPrefix(candidate, "-") || tofuargs.ConsumesValue(args[len(args)-2]) {
		return ""
	}
	return candidate
}

func workspacePath(workspaceDir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(workspaceDir, path)
}
