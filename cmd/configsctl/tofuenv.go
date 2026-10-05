package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"gopkg.in/yaml.v3"

	"goodkind.io/configsctl/internal/vault"
)

const settingsFile = "configsctl.yml"

const tofuVariablePrefix = "TF_VAR_"

type settings struct {
	Tofu tofuSettings `yaml:"tofu"`
}

type tofuSettings struct {
	// WorkspacesDir is a path relative to the repository root.
	WorkspacesDir string `yaml:"workspaces_dir"`
	// tofuSecretEnv exports a vault key <EnvKeyPrefix><NAME> as the
	// environment variable <NAME>.
	EnvKeyPrefix string `yaml:"env_key_prefix"`
}

func loadSettings(path string) (settings, error) {
	var loaded settings
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("settings file read failed", "path", path, "err", err)
		return loaded, fmt.Errorf("read %s: %w", path, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&loaded); err != nil {
		slog.Error("settings file parse failed", "path", path, "err", err)
		return loaded, fmt.Errorf("parse %s: %w", path, err)
	}
	if loaded.Tofu.WorkspacesDir == "" {
		return loaded, errors.New(path + ": tofu.workspaces_dir is empty")
	}
	if loaded.Tofu.EnvKeyPrefix == "" {
		return loaded, errors.New(path + ": tofu.env_key_prefix is empty")
	}
	return loaded, nil
}

var tofuVariableSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{{Type: "variable", LabelNames: []string{"name"}}},
}

var tofuTerraformSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{{Type: "terraform"}},
}

var tofuBackendSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{{Type: "backend", LabelNames: []string{"type"}}},
}

func tofuModuleFiles(dir string) (native, jsonFiles []string, err error) {
	native, err = filepath.Glob(filepath.Join(dir, "*.tf"))
	if err != nil {
		slog.Error("tofu module glob failed", "dir", dir, "err", err)
		return nil, nil, fmt.Errorf("list %s: %w", dir, err)
	}
	jsonFiles, err = filepath.Glob(filepath.Join(dir, "*.tf.json"))
	if err != nil {
		slog.Error("tofu module glob failed", "dir", dir, "err", err)
		return nil, nil, fmt.Errorf("list %s: %w", dir, err)
	}
	return native, jsonFiles, nil
}

// tofuModuleBlocks returns the top-level blocks that match schema in the *.tf
// and *.tf.json files of dir. It does not read subdirectories.
func tofuModuleBlocks(dir string, schema *hcl.BodySchema) (hcl.Blocks, error) {
	native, jsonFiles, err := tofuModuleFiles(dir)
	if err != nil {
		return nil, err
	}
	parser := hclparse.NewParser()
	var blocks hcl.Blocks
	for index, path := range append(native, jsonFiles...) {
		parse := parser.ParseHCLFile
		if index >= len(native) {
			parse = parser.ParseJSONFile
		}
		file, diags := parse(path)
		if diags.HasErrors() {
			slog.Error("tofu module parse failed", "path", path, "err", diags.Error())
			return nil, fmt.Errorf("parse %s: %w", path, diags)
		}
		content, _, diags := file.Body.PartialContent(schema)
		if diags.HasErrors() {
			slog.Error("tofu block read failed", "path", path, "err", diags.Error())
			return nil, fmt.Errorf("read blocks in %s: %w", path, diags)
		}
		blocks = append(blocks, content.Blocks...)
	}
	return blocks, nil
}

func tofuVariableNames(dir string) ([]string, error) {
	blocks, err := tofuModuleBlocks(dir, tofuVariableSchema)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(blocks))
	for _, block := range blocks {
		names = append(names, block.Labels[0])
	}
	sort.Strings(names)
	return names, nil
}

func tofuHasBackend(dir string) (bool, error) {
	blocks, err := tofuModuleBlocks(dir, tofuTerraformSchema)
	if err != nil {
		return false, err
	}
	for _, block := range blocks {
		content, _, diags := block.Body.PartialContent(tofuBackendSchema)
		if diags.HasErrors() {
			slog.Error("tofu backend read failed", "dir", dir, "err", diags.Error())
			return false, fmt.Errorf("read terraform block in %s: %w", dir, diags)
		}
		if len(content.Blocks) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func tofuChildWorkspaces(workspacesDir string) ([]string, error) {
	entries, err := os.ReadDir(workspacesDir)
	if err != nil {
		slog.Error("tofu workspaces directory read failed", "dir", workspacesDir, "err", err)
		return nil, fmt.Errorf("read %s: %w", workspacesDir, err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		hasBackend, err := tofuHasBackend(filepath.Join(workspacesDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		if hasBackend {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// selectTofuWorkspace returns the directory OpenTofu runs in and the arguments
// OpenTofu receives. A first argument equal to a child workspace name selects
// that child and is removed from the arguments. Any other first argument
// selects workspacesDir itself, which must have a backend block.
func selectTofuWorkspace(workspacesDir string, args []string) (string, []string, error) {
	names, err := tofuChildWorkspaces(workspacesDir)
	if err != nil {
		return "", nil, err
	}
	if len(args) > 0 {
		for _, name := range names {
			if name == args[0] {
				// The path uses the name os.ReadDir returned, not the argument.
				return filepath.Join(workspacesDir, name), args[1:], nil
			}
		}
	}
	hasBackend, err := tofuHasBackend(workspacesDir)
	if err != nil {
		return "", nil, err
	}
	if hasBackend {
		return workspacesDir, args, nil
	}
	if len(names) == 0 {
		return "", nil, fmt.Errorf(
			"no workspace found: %s and its child directories have no backend block",
			workspacesDir,
		)
	}
	return "", nil, fmt.Errorf(
		"%s has no backend block; pass one of these workspaces as the first argument: %s",
		workspacesDir, strings.Join(names, ", "),
	)
}

// tofuSecretEnv exports a vault key under two rules. A vault key with the
// name of a variable declared in workspaceDir exports as TF_VAR_<key>. A vault
// key <EnvKeyPrefix><NAME> exports as <NAME>.
func tofuSecretEnv(tofu tofuSettings, workspaceDir, vaultFile, passwordFile string) ([]string, error) {
	names, err := tofuVariableNames(workspaceDir)
	if err != nil {
		return nil, err
	}
	values, err := vault.Values(vaultFile, passwordFile)
	if err != nil {
		slog.Error("vault read for tofu variables failed", "err", err)
		return nil, fmt.Errorf("read vault for tofu variables: %w", err)
	}
	var assignments []string
	exportedAsVariable := make(map[string]bool, len(names))
	for _, name := range names {
		value, ok := values[name]
		if !ok {
			continue
		}
		assignments = append(assignments, tofuVariablePrefix+name+"="+value)
		exportedAsVariable[name] = true
	}
	var envKeys []string
	for key := range values {
		// The loop above already exported this vault key as TF_VAR_<key>.
		if exportedAsVariable[key] {
			continue
		}
		if name, ok := strings.CutPrefix(key, tofu.EnvKeyPrefix); ok && name != "" {
			envKeys = append(envKeys, key)
		}
	}
	sort.Strings(envKeys)
	for _, key := range envKeys {
		assignments = append(assignments, strings.TrimPrefix(key, tofu.EnvKeyPrefix)+"="+values[key])
	}
	return assignments, nil
}
