package installedsource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"goodkind.io/configsctl/internal/procgroup"
	"goodkind.io/configsctl/internal/tofuargs"
)

// ModuleCacheRequest selects the verified root and its persistent runtime data.
type ModuleCacheRequest struct {
	Binary            string
	Workspace         string
	CopyRoot          string
	DataDirectory     string
	WorkspaceSelector string
	AllowMissing      bool
}

type moduleCacheDocument struct {
	Modules []moduleCacheRecord `json:"Modules"`
}

type moduleCacheRecord struct {
	Key     string `json:"Key"`
	Source  string `json:"Source"`
	Version string `json:"Version"`
	Dir     string `json:"Dir"`
}

// CheckModuleCache compares cached resolutions with a fresh OpenTofu resolution.
// It does not write the persistent backend, selected workspace, or state.
// CheckModuleSources must validate the selected source context before this call.
func (s *Source) CheckModuleCache(ctx context.Context, request ModuleCacheRequest) error {
	copyRoot, err := filepath.EvalSymlinks(request.CopyRoot)
	if err != nil {
		slog.Error("tofu.source.module_copy_root_failed", "err", err)
		return fmt.Errorf("resolve the private source copy: %w", err)
	}

	request.CopyRoot = copyRoot
	original, err := s.Path(request.Workspace)
	if err != nil {
		return err
	}

	relative, err := filepath.Rel(s.Identity.Root, original)
	if err != nil {
		slog.Error("tofu.source.module_root_failed", "err", err)
		return fmt.Errorf("resolve the verified module root: %w", err)
	}

	root := filepath.Join(request.CopyRoot, relative)
	expected, err := resolvePrivateModules(ctx, request, root)
	if err != nil {
		return err
	}

	for _, record := range expected.Modules {
		if err := s.checkResolvedModule(request.CopyRoot, root, record.Dir); err != nil {
			return err
		}
	}

	path := filepath.Join(request.DataDirectory, "modules", "modules.json")
	current, err := readModuleManifest(path)
	if errors.Is(err, os.ErrNotExist) {
		if request.AllowMissing || len(expected.Modules) <= 1 {
			return nil
		}

		return fmt.Errorf(
			"run init to record verified local module resolutions before running OpenTofu: %w",
			err,
		)
	}

	if err != nil {
		return fmt.Errorf("read persistent module resolutions before OpenTofu execution: %w", err)
	}

	if err := compareModuleManifests(root, expected, current); err != nil {
		return fmt.Errorf(
			"remove only %s and rerun init; cached module resolutions do not match verified source: %w",
			path,
			err,
		)
	}

	slog.Debug("tofu.source.module_cache_verified", "workspace", original)

	return nil
}

func resolvePrivateModules(
	ctx context.Context,
	request ModuleCacheRequest,
	root string,
) (moduleCacheDocument, error) {
	directory, err := os.MkdirTemp(request.CopyRoot, "module-resolution-*")
	if err != nil {
		slog.Error("tofu.source.module_directory_failed", "err", err)
		return moduleCacheDocument{}, fmt.Errorf("create private module resolution data: %w", err)
	}
	defer removeModuleDirectory(directory)
	// #nosec G204 -- Binary uses the existing absolute executable selection.
	command := exec.CommandContext(
		ctx,
		request.Binary,
		tofuargs.WithWorkspaceSelector([]string{"get", "-no-color"}, request.WorkspaceSelector)...,
	)
	command.Dir = root
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + directory,
		"TF_DATA_DIR=" + directory,
		"TF_IN_AUTOMATION=1",
	}
	if request.WorkspaceSelector != "" {
		command.Env = append(command.Env, "TF_VAR_workspace="+request.WorkspaceSelector)
	}

	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := procgroup.RunGroup(command); err != nil {
		slog.Error("tofu.source.module_resolution_failed", "err", err)

		return moduleCacheDocument{}, fmt.Errorf(
			"resolve verified local modules with tofu get: %w",
			err,
		)
	}

	return readModuleManifest(filepath.Join(directory, "modules", "modules.json"))
}

func removeModuleDirectory(path string) {
	if err := os.RemoveAll(path); err != nil {
		slog.Warn("tofu.source.module_directory_remove_failed", "err", err)
	}
}

func readModuleManifest(path string) (moduleCacheDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Error("tofu.source.module_manifest_read_failed", "path", path, "err", err)
		}

		return moduleCacheDocument{}, fmt.Errorf("read OpenTofu module manifest: %w", err)
	}

	var document moduleCacheDocument
	if err := json.Unmarshal(data, &document); err != nil {
		slog.Error("tofu.source.module_manifest_decode_failed", "path", path, "err", err)
		return moduleCacheDocument{}, fmt.Errorf("decode OpenTofu module manifest: %w", err)
	}

	return document, nil
}

func (s *Source) checkResolvedModule(copyRoot, root, directory string) error {
	path, err := resolvedModuleDirectory(root, directory)
	if err != nil {
		return err
	}

	relative, err := filepath.Rel(copyRoot, path)
	if err != nil || !filepath.IsLocal(relative) {
		return errors.New("OpenTofu resolved a module outside the verified source copy")
	}

	original := filepath.Join(s.Identity.Root, relative)
	for _, tree := range s.trees {
		inside, err := filepath.Rel(tree, original)
		if err == nil && filepath.IsLocal(inside) {
			return nil
		}
	}

	return errors.New("OpenTofu resolved a module outside a complete verified input tree")
}

func compareModuleManifests(root string, expected, current moduleCacheDocument) error {
	if len(expected.Modules) != len(current.Modules) {
		return errors.New("the cached module set changed")
	}

	actual := make(map[string]moduleCacheRecord, len(current.Modules))
	for _, record := range current.Modules {
		if _, duplicate := actual[record.Key]; duplicate {
			return errors.New("the module cache repeats a module key")
		}

		actual[record.Key] = record
	}

	for _, record := range expected.Modules {
		cached, present := actual[record.Key]
		if !present || cached.Source != record.Source || cached.Version != record.Version {
			return fmt.Errorf("module %s does not match its verified source", record.Key)
		}

		wanted, err := resolvedModuleDirectory(root, record.Dir)
		if err != nil {
			return err
		}

		observed, err := resolvedModuleDirectory(root, cached.Dir)
		if err != nil || observed != wanted {
			return fmt.Errorf("module %s resolves outside its verified directory", record.Key)
		}
	}

	return nil
}

func resolvedModuleDirectory(root, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}

	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		slog.Error("tofu.source.module_path_failed", "err", err)
		return "", fmt.Errorf("resolve the OpenTofu module directory: %w", err)
	}

	return canonical, nil
}
