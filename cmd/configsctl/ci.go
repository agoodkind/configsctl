package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"goodkind.io/configsctl/tofurun"
)

const ciUsage = "usage: configsctl ci tofu --config <file> --root <root> --workspace <workspace> -- <tofu args...>"

// ciConfigVersion is the only supported version of the ci tofu configuration.
const ciConfigVersion = 1

// The keys of the ci tofu configuration. json_path is optional.
const (
	ciVersionKey             = "version"
	ciRequiredEnvironmentKey = "required_environment"
	ciSecretEnvironmentKey   = "secret_environment"
	ciSecretFilesKey         = "secret_files"
	ciOutputModeKey          = "output_mode"
	ciJSONPathKey            = "json_path"
)

var ciRequiredKeys = []string{
	ciVersionKey, ciRequiredEnvironmentKey, ciSecretEnvironmentKey, ciSecretFilesKey, ciOutputModeKey,
}

// ciConfig represents the ci tofu configuration. The environment lists
// contain variable names and do not contain values.
type ciConfig struct {
	Version             int               `json:"version"`
	RequiredEnvironment []string          `json:"required_environment"`
	SecretEnvironment   []string          `json:"secret_environment"`
	SecretFiles         map[string]string `json:"secret_files"`
	OutputMode          string            `json:"output_mode"`
	JSONPath            string            `json:"json_path"`
}

// ciTofuOptions are the command line options of ci tofu.
type ciTofuOptions struct {
	config    string
	root      string
	workspace string
	tofuArgs  []string
}

// runCI runs the ci subcommands. ci tofu runs one OpenTofu command without a
// host lock scope, and tofurun.Run refuses a mutation without a scope. ci
// does not read the vault or the vault password file.
func runCI(args []string) error {
	if len(args) == 0 || args[0] != "tofu" {
		return errors.New(ciUsage)
	}
	options, err := parseCITofu(args[1:])
	if err != nil {
		return err
	}
	config, err := readCIConfig(options.config)
	if err != nil {
		return err
	}
	root, err := checkoutRoot(options.root)
	if err != nil {
		return err
	}
	binary, err := tofuBinary()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	request := tofurun.Request{
		Binary: binary, Root: root, Workspace: options.workspace,
		Args: options.tofuArgs, Environment: os.Environ(),
		RequiredEnvironment: config.RequiredEnvironment, SecretEnvironment: config.SecretEnvironment,
		SecretFiles: config.SecretFiles, OutputMode: tofurun.OutputMode(config.OutputMode),
		JSONPath: config.JSONPath, RequiredHosts: nil,
	}
	result, err := tofurun.Run(ctx, request, nil)
	if err != nil {
		slog.Error("ci.tofu_failed", "root", root, "workspace", options.workspace, "err", err)
		return fmt.Errorf("ci tofu: %w", err)
	}
	if result.ExitCode != 0 {
		return &exitCodeError{code: result.ExitCode}
	}
	return nil
}

func parseCITofu(args []string) (ciTofuOptions, error) {
	options := ciTofuOptions{config: "", root: "", workspace: "", tofuArgs: nil}
	values := map[string]*string{"--config": &options.config, "--root": &options.root, "--workspace": &options.workspace}
	pending, separated := "", false
	for _, argument := range args {
		switch {
		case separated:
			options.tofuArgs = append(options.tofuArgs, argument)
			continue
		case pending != "":
			*values[pending] = argument
			pending = ""
			continue
		case argument == "--":
			separated = true
			continue
		}
		name, value, inline := strings.Cut(argument, "=")
		target, known := values[name]
		if !known || *target != "" {
			return options, fmt.Errorf("ci tofu does not recognize argument %q or received it more than once; %s", argument, ciUsage)
		}
		if inline {
			*target = value
		} else {
			pending = name
		}
	}
	if options.config == "" || options.root == "" || options.workspace == "" || len(options.tofuArgs) == 0 {
		return options, errors.New(ciUsage)
	}
	return options, nil
}

// readCIConfig reads the strict JSON configuration. It rejects unknown,
// duplicate, missing, and null fields, trailing data, and an unsupported
// version or output mode.
func readCIConfig(path string) (ciConfig, error) {
	var config ciConfig
	body, err := os.ReadFile(path)
	if err != nil {
		slog.Error("ci.config_unreadable", "path", path, "err", err)
		return config, fmt.Errorf("read the ci tofu configuration: %w", err)
	}
	keys, err := strictJSONKeys(body)
	if err != nil {
		slog.Error("ci.config_invalid", "path", path, "err", err)
		return config, fmt.Errorf("ci tofu configuration %s: %w", path, err)
	}
	for _, key := range ciRequiredKeys {
		if !slices.Contains(keys, key) {
			return config, ciConfigError(path, fmt.Sprintf("the configuration requires the field %q", key))
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		slog.Error("ci.config_invalid", "path", path, "err", err)
		return config, fmt.Errorf("ci tofu configuration %s: %w", path, err)
	}
	if config.Version != ciConfigVersion {
		return config, ciConfigError(path, fmt.Sprintf("version must be %d", ciConfigVersion))
	}
	switch tofurun.OutputMode(config.OutputMode) {
	case tofurun.RedactedProgress:
		if slices.Contains(keys, ciJSONPathKey) {
			return config, ciConfigError(path, "json_path requires output_mode private_json")
		}
	case tofurun.PrivateJSON:
		if config.JSONPath == "" {
			return config, ciConfigError(path, "output_mode private_json requires json_path")
		}
	default:
		return config, ciConfigError(path, fmt.Sprintf("the configuration does not support output_mode %q", config.OutputMode))
	}
	return config, nil
}

func ciConfigError(path, reason string) error {
	err := fmt.Errorf("ci tofu configuration %s: %s", path, reason)
	slog.Error("ci.config_invalid", "path", path, "err", err)
	return err
}

// strictJSONKeys returns the keys of the top-level JSON object in body. It
// rejects another top-level value, an unknown top-level key, a duplicate key
// in any object, a null value, and trailing data.
func strictJSONKeys(body []byte) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	start, err := decoder.Token()
	if err != nil {
		return nil, ciJSONError("read the configuration object", err)
	}
	if start != json.Delim('{') {
		return nil, errors.New("the configuration must be one JSON object")
	}
	keys, err := readObjectKeys(decoder)
	if err != nil {
		return nil, err
	}
	allowed := append(slices.Clone(ciRequiredKeys), ciJSONPathKey)
	for _, key := range keys {
		if !slices.Contains(allowed, key) {
			return nil, fmt.Errorf("the field %q is unknown", key)
		}
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("the configuration has data after its JSON object")
	}
	return keys, nil
}

// readObjectKeys reads the members of an object after its opening brace and
// returns its keys.
func readObjectKeys(decoder *json.Decoder) ([]string, error) {
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, ciJSONError("read a configuration key", err)
		}
		key, isKey := token.(string)
		if !isKey {
			return nil, errors.New("the configuration has an invalid object key")
		}
		if slices.Contains(keys, key) {
			return nil, fmt.Errorf("the field %q appears more than once", key)
		}
		keys = append(keys, key)
		if err := skipJSONValue(decoder); err != nil {
			return nil, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, ciJSONError("read the end of a configuration object", err)
	}
	return keys, nil
}

// skipJSONValue reads one value and checks every object inside it.
func skipJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return ciJSONError("read a configuration value", err)
	}
	switch token {
	case nil:
		return errors.New("the configuration contains a null value")
	case json.Delim('{'):
		_, err := readObjectKeys(decoder)
		return err
	case json.Delim('['):
		for decoder.More() {
			if err := skipJSONValue(decoder); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return ciJSONError("read the end of a configuration array", err)
		}
	}
	return nil
}

// ciJSONError logs and wraps a JSON syntax error in the configuration.
func ciJSONError(operation string, err error) error {
	wrapped := fmt.Errorf("%s: %w", operation, err)
	slog.Error("ci.config_syntax_invalid", "err", wrapped)
	return wrapped
}

// checkoutRoot resolves root below the working directory, which is the
// configuration checkout.
func checkoutRoot(root string) (string, error) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		slog.Error("ci.working_directory_failed", "err", err)
		return "", fmt.Errorf("read the working directory: %w", err)
	}
	checkout, err := filepath.EvalSymlinks(workingDirectory)
	if err != nil {
		slog.Error("ci.working_directory_failed", "err", err)
		return "", fmt.Errorf("resolve the working directory: %w", err)
	}
	candidate := root
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(checkout, candidate)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		slog.Error("ci.root_unresolved", "root", root, "err", err)
		return "", fmt.Errorf("resolve the OpenTofu root %s: %w", root, err)
	}
	relative, err := filepath.Rel(checkout, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		err = fmt.Errorf("the OpenTofu root %s is outside the checkout %s", root, checkout)
		slog.Error("ci.root_refused", "root", root, "err", err)
		return "", err
	}
	return resolved, nil
}

// tofuBinary returns the absolute path of tofu from PATH.
func tofuBinary() (string, error) {
	found, err := exec.LookPath("tofu")
	if err != nil {
		slog.Error("ci.tofu_missing", "err", err)
		return "", fmt.Errorf("find tofu in PATH: %w", err)
	}
	binary, err := filepath.Abs(found)
	if err != nil {
		slog.Error("ci.tofu_missing", "path", found, "err", err)
		return "", fmt.Errorf("resolve the tofu path %s: %w", found, err)
	}
	return binary, nil
}
