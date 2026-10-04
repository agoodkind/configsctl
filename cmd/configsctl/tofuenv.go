package main

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"

	"goodkind.io/configsctl/internal/vault"
)

// tofuVariablePrefix is the environment prefix OpenTofu reads variable values from.
const tofuVariablePrefix = "TF_VAR_"

// tofuVariableSchema selects the variable blocks of a module file and ignores
// every other block.
var tofuVariableSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{{Type: "variable", LabelNames: []string{"name"}}},
}

// tofuVariableNames returns the sorted label of every variable block in the
// module in dir.
func tofuVariableNames(dir string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.tf"))
	if err != nil {
		slog.Error("tofu module glob failed", "dir", dir, "err", err)
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	parser := hclparse.NewParser()
	var names []string
	for _, path := range paths {
		file, diags := parser.ParseHCLFile(path)
		if diags.HasErrors() {
			slog.Error("tofu module parse failed", "path", path, "err", diags.Error())
			return nil, fmt.Errorf("parse %s: %w", path, diags)
		}
		content, _, diags := file.Body.PartialContent(tofuVariableSchema)
		if diags.HasErrors() {
			slog.Error("tofu variable read failed", "path", path, "err", diags.Error())
			return nil, fmt.Errorf("read variables in %s: %w", path, diags)
		}
		for _, block := range content.Blocks {
			names = append(names, block.Labels[0])
		}
	}
	sort.Strings(names)
	return names, nil
}

// tofuSecretEnv returns one TF_VAR assignment per vault key that the module in
// dir declares as a variable of the same name. A vault key without a matching
// variable is not exported.
func tofuSecretEnv(dir, vaultFile, passwordFile string) ([]string, error) {
	names, err := tofuVariableNames(dir)
	if err != nil {
		return nil, err
	}
	values, err := vault.Values(vaultFile, passwordFile)
	if err != nil {
		slog.Error("vault read for tofu variables failed", "err", err)
		return nil, fmt.Errorf("read vault for tofu variables: %w", err)
	}
	var assignments []string
	for _, name := range names {
		value, ok := values[name]
		if !ok {
			continue
		}
		assignments = append(assignments, tofuVariablePrefix+name+"="+value)
	}
	return assignments, nil
}
