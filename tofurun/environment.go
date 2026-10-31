package tofurun

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"goodkind.io/configsctl/hostlock"
	"goodkind.io/configsctl/internal/redact"
)

// Run can include these ordinary process settings in the child environment.
const (
	pathVariable       = "PATH"
	homeVariable       = "HOME"
	tmpdirVariable     = "TMPDIR"
	langVariable       = "LANG"
	automationVariable = "TF_IN_AUTOMATION"
	automationValue    = "1"
)

var ordinaryVariables = []string{pathVariable, homeVariable, tmpdirVariable, langVariable, automationVariable}

// Run rejects declared variables with these prefixes or names. They include
// GitHub and Actions credentials, action inputs, Cloudflare Access
// credentials, Ansible vault unlock settings, OpenTofu argument and logging
// overrides, workspace and data directory overrides, pre-run hooks, and the
// host lock state directory.
var (
	excludedPrefixes  = []string{"GITHUB_", "GH_", "ACTIONS_", "INPUT_", "CF_ACCESS_", "ANSIBLE_VAULT", "TF_CLI_ARGS", "TF_LOG"}
	excludedVariables = []string{"TERRAFORM_PRE_RUN", "TF_WORKSPACE", "TF_DATA_DIR", hostlock.StateEnvVar}
)

var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// environment is the child environment and the secret values to redact.
type environment struct {
	values   []string
	patterns []redact.Pattern
	tmpdir   string
}

func buildEnvironment(request Request) (environment, error) {
	built := environment{values: nil, patterns: nil, tmpdir: ""}
	declared, err := declaredVariables(request)
	if err != nil {
		return built, err
	}
	allowed := slices.Concat(ordinaryVariables, declared)
	assigned, err := assignments(request.Environment, allowed)
	if err != nil {
		return built, err
	}
	if err := checkOrdinary(assigned); err != nil {
		return built, err
	}
	built.tmpdir = assigned[tmpdirVariable]
	for _, name := range request.RequiredEnvironment {
		if assigned[name] == "" {
			return built, rejection("tofurun.variable_missing", "the environment does not set the required variable "+name)
		}
	}
	for _, name := range request.SecretEnvironment {
		pattern, err := secretPattern(name, []byte(assigned[name]))
		if err != nil {
			return built, err
		}
		built.patterns = append(built.patterns, pattern)
	}
	filePatterns, err := secretFilePatterns(request.SecretFiles, assigned)
	if err != nil {
		return built, err
	}
	built.patterns = append(built.patterns, filePatterns...)
	for _, name := range allowed {
		if value, ok := assigned[name]; ok {
			built.values = append(built.values, name+"="+value)
		}
	}
	return built, nil
}

// declaredVariables returns the names in RequiredEnvironment,
// SecretEnvironment, and SecretFiles after checking each name.
func declaredVariables(request Request) ([]string, error) {
	fileNames := make([]string, 0, len(request.SecretFiles))
	for name := range request.SecretFiles {
		fileNames = append(fileNames, name)
	}
	slices.Sort(fileNames)
	declared := slices.Concat(request.RequiredEnvironment, request.SecretEnvironment, fileNames)
	for index, name := range declared {
		if err := checkVariableName(name); err != nil {
			return nil, err
		}
		if slices.Contains(declared[:index], name) {
			return nil, rejection("tofurun.variable_duplicate", fmt.Sprintf("the request declares the variable %s more than once", name))
		}
	}
	return declared, nil
}

func checkVariableName(name string) error {
	if !variableName.MatchString(name) {
		return rejection("tofurun.variable_refused", fmt.Sprintf("%q is not an environment variable name", name))
	}
	if slices.Contains(ordinaryVariables, name) {
		return rejection("tofurun.variable_refused", fmt.Sprintf("the ordinary setting %s cannot be declared", name))
	}
	if slices.Contains(excludedVariables, name) {
		return rejection("tofurun.variable_refused", fmt.Sprintf("the variable %s is not permitted", name))
	}
	for _, prefix := range excludedPrefixes {
		if strings.HasPrefix(name, prefix) {
			return rejection("tofurun.variable_refused", fmt.Sprintf("the variable %s is not permitted", name))
		}
	}
	return nil
}

// assignments returns the values of names in source and rejects a name that
// source assigns more than once.
func assignments(source, names []string) (map[string]string, error) {
	assigned := map[string]string{}
	for _, entry := range source {
		name, value, found := strings.Cut(entry, "=")
		if !found || !slices.Contains(names, name) {
			continue
		}
		if _, seen := assigned[name]; seen {
			return nil, rejection("tofurun.variable_duplicate", fmt.Sprintf("the environment assigns %s more than once", name))
		}
		assigned[name] = value
	}
	return assigned, nil
}

func checkOrdinary(assigned map[string]string) error {
	if assigned[pathVariable] == "" {
		return rejection("tofurun.variable_missing", "the environment does not set PATH")
	}
	if assigned[automationVariable] != automationValue {
		return rejection("tofurun.variable_missing", "the environment must set TF_IN_AUTOMATION=1")
	}
	for _, name := range []string{homeVariable, tmpdirVariable} {
		if err := checkPrivateDirectory(name, assigned[name]); err != nil {
			return err
		}
	}
	return nil
}

func secretPattern(name string, value []byte) (redact.Pattern, error) {
	pattern := redact.Pattern{Value: value, Label: name}
	if len(value) == 0 {
		return pattern, rejection("tofurun.secret_missing", "the environment does not set the secret "+name)
	}
	if _, ok := redact.Validate([]redact.Pattern{pattern}); !ok {
		reason := fmt.Sprintf("the secret %s must contain at least %d bytes for redaction", name, redact.MinLen)
		return pattern, rejection("tofurun.secret_unmaskable", reason)
	}
	return pattern, nil
}

func secretFilePatterns(files map[string]string, assigned map[string]string) ([]redact.Pattern, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	patterns := make([]redact.Pattern, 0, len(names))
	for _, name := range names {
		path := files[name]
		if assigned[name] != path {
			return nil, rejection("tofurun.secret_file_unbound", fmt.Sprintf("the environment must set %s to %s", name, path))
		}
		contents, err := readSecretFile(name, path)
		if err != nil {
			return nil, err
		}
		pattern, err := secretPattern(name, contents)
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, pattern)
	}
	return patterns, nil
}

// checkSecretArguments rejects an argument that contains a secret value.
func checkSecretArguments(args []string, patterns []redact.Pattern) error {
	for index, argument := range args {
		for _, pattern := range patterns {
			if bytes.Contains([]byte(argument), pattern.Value) {
				return rejection("tofurun.secret_argument", fmt.Sprintf("argument %d contains the secret %s", index, pattern.Label))
			}
		}
	}
	return nil
}

// readSecretFile reads a private regular file in a private directory.
func readSecretFile(name, path string) ([]byte, error) {
	if err := checkPrivateFile(name, path); err != nil {
		return nil, err
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, rejection("tofurun.secret_file_unreadable", fmt.Sprintf("read the secret file of %s: %v", name, err))
	}
	return contents, nil
}
