package main

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"goodkind.io/configsctl/internal/vault"
)

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
