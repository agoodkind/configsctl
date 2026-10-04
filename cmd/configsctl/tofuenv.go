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
	// ModuleDir is a path relative to the repository root.
	ModuleDir string `yaml:"module_dir"`
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
	if loaded.Tofu.ModuleDir == "" {
		return loaded, errors.New(path + ": tofu.module_dir is empty")
	}
	if loaded.Tofu.EnvKeyPrefix == "" {
		return loaded, errors.New(path + ": tofu.env_key_prefix is empty")
	}
	return loaded, nil
}

var tofuVariableSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{{Type: "variable", LabelNames: []string{"name"}}},
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

func tofuVariableNames(dir string) ([]string, error) {
	native, jsonFiles, err := tofuModuleFiles(dir)
	if err != nil {
		return nil, err
	}
	parser := hclparse.NewParser()
	var names []string
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

// tofuSecretEnv exports a vault key under two rules. A vault key with the
// name of a variable declared in the module exports as TF_VAR_<key>. A vault
// key <EnvKeyPrefix><NAME> exports as <NAME>.
func tofuSecretEnv(tofu tofuSettings, vaultFile, passwordFile string) ([]string, error) {
	names, err := tofuVariableNames(tofu.ModuleDir)
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
		// A vault key exported as TF_VAR_<key> is not exported a second time
		// under the prefix rule.
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
