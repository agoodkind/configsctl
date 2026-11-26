// Package tofusecrets resolves credentials for OpenTofu commands.
package tofusecrets

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"goodkind.io/configsctl/internal/redact"
	"goodkind.io/configsctl/internal/vault"
)

const variable = "CONFIGSCTL_TOFU_SECRET_ENVIRONMENT"

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var (
	errInvalidNames = errors.New(
		"CONFIGSCTL_TOFU_SECRET_ENVIRONMENT must list valid, unique environment variable names",
	)
	errMissingInput    = errors.New("set the required OpenTofu secret environment variable")
	errUnmaskableInput = errors.New(
		"the OpenTofu secret environment variable is too short to redact",
	)
)

// Environment contains the protected variables for an OpenTofu invocation.
type Environment struct {
	patterns []redact.Pattern
}

// ReadEnvironment selects environment secrets for the tofu command.
//
// It reads the named values at startup so the caller can redact all output.
// A nil result selects the legacy vault.
//
//	CONFIGSCTL_TOFU_SECRET_ENVIRONMENT=AWS_SECRET_ACCESS_KEY
func ReadEnvironment(command string) (*Environment, error) {
	if command != "tofu" {
		return nil, nil
	}

	list, configured := os.LookupEnv(variable)
	if !configured {
		return nil, nil
	}

	input := &Environment{patterns: nil}
	seen := make(map[string]bool)

	for part := range strings.SplitSeq(list, ",") {
		name := strings.TrimSpace(part)
		if !environmentName.MatchString(name) || seen[name] {
			return nil, errInvalidNames
		}

		seen[name] = true

		value := os.Getenv(name)
		if value == "" {
			slog.Error("OpenTofu secret input is missing", "variable", name, "err", errMissingInput)

			return nil, fmt.Errorf("%w %s", errMissingInput, name)
		}

		input.patterns = append(input.patterns, redact.Pattern{Label: name, Value: []byte(value)})
	}

	if name, valid := redact.Validate(input.patterns); !valid {
		return nil, fmt.Errorf(
			"%w: %s needs at least %d bytes",
			errUnmaskableInput,
			name,
			redact.MinLen,
		)
	}

	return input, nil
}

// Patterns returns the configured values for output redaction.
//
// Use the patterns before logging output from OpenTofu.
//
//	patterns := inputs.Patterns()
func (input *Environment) Patterns() []redact.Pattern {
	return input.patterns
}

// ReadCredential reads a named infrastructure credential.
//
// In environment mode, include the credential's TF_VAR_ name in the secret
// list. A nil environment uses the existing vault and password files.
//
//	CONFIGSCTL_TOFU_SECRET_ENVIRONMENT=TF_VAR_rotator_password
func (input *Environment) ReadCredential(key, vaultFile, passwordFile string) (string, error) {
	if input == nil {
		value, err := vault.Secret(key, vaultFile, passwordFile)
		if err != nil {
			slog.Error("Read the OpenTofu credential from the vault", "err", err)

			return "", fmt.Errorf("read the OpenTofu credential from the vault: %w", err)
		}

		return value, nil
	}

	name := tofuVariablePrefix + key

	for _, pattern := range input.patterns {
		if pattern.Label == name {
			return string(pattern.Value), nil
		}
	}

	return "", fmt.Errorf("%w %s and list it in %s", errMissingInput, name, variable)
}

func (input *Environment) assignments() []string {
	assignments := make([]string, 0, len(input.patterns))
	for _, pattern := range input.patterns {
		assignments = append(assignments, pattern.Label+"="+string(pattern.Value))
	}

	return assignments
}
