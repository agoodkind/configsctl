package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// tofuChildWorkspaces discovers backend roots below workspacesDir and returns
// their sorted relative selectors. It excludes shared modules and generated files.
func tofuChildWorkspaces(workspacesDir string) ([]string, error) {
	var names []string
	err := filepath.WalkDir(workspacesDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			slog.Error("tofu.workspace.directory_failed", "path", path, "err", walkErr)
			return fmt.Errorf("read workspace directory %s: %w", path, walkErr)
		}
		if !entry.IsDir() || path == workspacesDir {
			return nil
		}
		if excludedWorkspaceDirectory(entry.Name()) {
			return filepath.SkipDir
		}
		hasBackend, err := tofuHasBackend(path)
		if err != nil {
			return err
		}
		if hasBackend {
			relative, err := filepath.Rel(workspacesDir, path)
			if err != nil {
				return fmt.Errorf("resolve workspace %s relative to %s: %w", path, workspacesDir, err)
			}
			names = append(names, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		slog.Error("tofu.workspace.discovery_failed", "root", workspacesDir, "err", err)
		return nil, fmt.Errorf("discover workspaces below %s: %w", workspacesDir, err)
	}
	slices.Sort(names)
	return names, nil
}

func excludedWorkspaceDirectory(name string) bool {
	return strings.HasPrefix(name, ".") || name == "terraform.d" || name == "modules"
}

func validateWorkspaceSelector(selector string) error {
	if selector == "" || selector == "." || filepath.IsAbs(selector) || !filepath.IsLocal(selector) ||
		strings.Contains(selector, "\\") || filepath.ToSlash(filepath.Clean(selector)) != selector {
		return fmt.Errorf("workspace selector %q must be a clean relative path inside the workspace tree", selector)
	}
	if slices.ContainsFunc(strings.Split(selector, "/"), excludedWorkspaceDirectory) {
		return fmt.Errorf("workspace selector %q contains an excluded directory", selector)
	}
	return nil
}

// selectedWorkspaceDir checks only selector, without parsing unrelated roots.
// Each directory component must exist without following a symbolic link.
func selectedWorkspaceDir(workspacesDir, selector string) (string, bool, error) {
	if err := validateWorkspaceSelector(selector); err != nil {
		return "", false, err
	}
	path := workspacesDir
	for component := range strings.SplitSeq(selector, "/") {
		path = filepath.Join(path, component)
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		if err != nil {
			slog.Error("tofu.workspace.directory_failed", "path", path, "err", err)
			return "", false, fmt.Errorf("read workspace directory %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", false, fmt.Errorf("workspace directory %s must be a directory without symbolic links", path)
		}
	}
	hasBackend, err := tofuHasBackend(path)
	if err != nil {
		return "", false, err
	}
	if !hasBackend {
		return "", false, nil
	}
	return path, true, nil
}

// selectTofuWorkspace accepts exact relative selectors and preserves commands
// against the root backend. Selected roots do not depend on other roots parsing.
func selectTofuWorkspace(workspacesDir string, args []string) (string, []string, error) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		path, selected, err := selectedWorkspaceDir(workspacesDir, args[0])
		if err != nil {
			return "", nil, err
		}
		if selected {
			return path, args[1:], nil
		}
		if strings.Contains(args[0], "/") {
			return "", nil, fmt.Errorf("workspace %s does not exist", args[0])
		}
	}
	hasBackend, err := tofuHasBackend(workspacesDir)
	if err != nil {
		return "", nil, err
	}
	if hasBackend {
		return workspacesDir, args, nil
	}
	names, err := tofuChildWorkspaces(workspacesDir)
	if err != nil {
		return "", nil, err
	}
	if len(names) == 0 {
		return "", nil, fmt.Errorf("no workspace found: %s and its child directories have no backend block", workspacesDir)
	}
	return "", nil, fmt.Errorf("%s has no backend block; pass one of these workspaces as the first argument: %s",
		workspacesDir, strings.Join(names, ", "))
}
