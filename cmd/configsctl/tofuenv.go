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
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"gopkg.in/yaml.v3"

	"goodkind.io/configsctl/internal/vault"
)

// settingsFile is the file in the configs repository root that configures
// configsctl.
const settingsFile = "configsctl.yml"

// tofuVariablePrefix is the environment prefix OpenTofu itself defines for
// variable values.
const tofuVariablePrefix = "TF_VAR_"

// settings is the content of settingsFile.
type settings struct {
	Tofu tofuSettings `yaml:"tofu"`
}

// tofuSettings configures how configsctl runs OpenTofu.
type tofuSettings struct {
	// ModuleDir is the OpenTofu root module, relative to the repository root.
	ModuleDir string `yaml:"module_dir"`
	// EnvKeyPrefix marks a vault key that OpenTofu reads as a plain environment
	// variable. A vault key <EnvKeyPrefix><NAME> exports as <NAME>.
	EnvKeyPrefix string `yaml:"env_key_prefix"`
	// TOTPKeyPrefix marks a vault key with a TOTP seed. A vault key
	// <TOTPKeyPrefix><name> exports the current code as the OpenTofu variable
	// <name> when the module declares that variable. Empty turns the rule off.
	TOTPKeyPrefix string `yaml:"totp_key_prefix"`
}

// loadSettings reads and validates the settings file at path. An unknown key
// or a missing value is an error.
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

// tofuVariableSchema selects the variable blocks of a module file and ignores
// every other block.
var tofuVariableSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{{Type: "variable", LabelNames: []string{"name"}}},
}

// tofuModuleFiles returns the native and JSON module files in dir.
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

// tofuVariableNames returns the sorted label of every variable block in the
// module in dir.
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

// tofuSecretEnv returns the environment assignments OpenTofu gets from the
// vault. A vault key with the same name as a variable declared in the module
// exports as TF_VAR_<key>. A vault key <TOTPKeyPrefix><name> exports the TOTP
// code for now as TF_VAR_<name> when the module declares <name>. A vault key
// <EnvKeyPrefix><NAME> exports as <NAME>. Every other vault key is not exported.
func tofuSecretEnv(tofu tofuSettings, vaultFile, passwordFile string, now time.Time) ([]string, error) {
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
		if value, ok := values[name]; ok {
			assignments = append(assignments, tofuVariablePrefix+name+"="+value)
			exportedAsVariable[name] = true
			continue
		}
		if tofu.TOTPKeyPrefix == "" {
			continue
		}
		seed, ok := values[tofu.TOTPKeyPrefix+name]
		if !ok {
			continue
		}
		code, codeErr := totpCode(seed, now)
		if codeErr != nil {
			return nil, fmt.Errorf("vault key %s%s: %w", tofu.TOTPKeyPrefix, name, codeErr)
		}
		assignments = append(assignments, tofuVariablePrefix+name+"="+code)
	}
	var envKeys []string
	for key := range values {
		// A key that a variable already claimed exports once, as that variable.
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
