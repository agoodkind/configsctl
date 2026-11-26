package main

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"goodkind.io/configsctl/internal/tofufiles"
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

var tofuOperationalSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "resource", LabelNames: []string{"type", "name"}},
		{Type: "data", LabelNames: []string{"type", "name"}},
		{Type: "module", LabelNames: []string{"name"}},
	},
}

func tofuHasOperationalConfiguration(dir string) (bool, error) {
	blocks, err := tofuModuleBlocks(dir, tofuOperationalSchema)
	if err != nil {
		return false, err
	}

	return len(blocks) > 0, nil
}

func tofuModuleFiles(dir string) (native, jsonFiles []string, err error) {
	native, jsonFiles, err = tofufiles.Discover(dir)
	if err != nil {
		slog.Error("tofu module discovery failed", "dir", dir, "err", err)
		return nil, nil, fmt.Errorf("list %s: %w", dir, err)
	}

	return native, jsonFiles, nil
}

// tofuModuleBlocks reads matching blocks from the active source files of dir.
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
func tofuSecretEnv(
	tofu tofuSettings,
	workspaceDir string,
	vaultFile string,
	passwordFile string,
) ([]string, error) {
	if tofu.source != nil {
		workspaceDir = tofu.source.path(workspaceDir)
	}

	names, err := tofuVariableNames(workspaceDir)
	if err != nil {
		return nil, err
	}

	assignments, err := tofu.runtimeSecrets.ReadAssignments(
		names,
		tofu.EnvKeyPrefix,
		vaultFile,
		passwordFile,
	)
	if err != nil {
		slog.Error("read OpenTofu secret inputs", "err", err)

		return nil, fmt.Errorf("read OpenTofu secret inputs: %w", err)
	}

	if tofu.ProviderMirror != nil {
		configuration, err := tofuProviderConfig(*tofu.ProviderMirror)
		if err != nil {
			return nil, err
		}

		assignments = append(assignments, "TF_CLI_CONFIG_FILE="+configuration)
	}

	return assignments, nil
}
