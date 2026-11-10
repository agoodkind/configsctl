package main

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"goodkind.io/configsctl/internal/redact"
)

type operatorInputs struct {
	home        string
	assignments []string
	publicNames []string
}

func registerOperatorRedaction(env cmdEnv, settings tofuSettings, workspace string, args, environment []string) (cmdEnv, func(), error) {
	if !tofuMutates(args) || strictExplicitSettings(settings, settings.LockTargets[tofuWorkspaceKey(workspace)]) {
		return env, func() {}, nil
	}
	inputWorkspace := workspace
	if settings.source != nil {
		inputWorkspace = settings.source.path(workspace)
	}
	protection, err := tofuVariableProtection(inputWorkspace)
	if err != nil {
		return env, nil, err
	}
	var patterns []redact.Pattern
	for _, name := range slices.Sorted(maps.Keys(protection)) {
		if !protection[name] {
			continue
		}
		key := tofuVariablePrefix + name
		value, present := assignedEnvironmentValue(environment, key)
		if present {
			patterns = append(patterns, redact.Pattern{Label: key, Value: []byte(value)})
		}
	}
	if key, valid := redact.Validate(patterns); !valid {
		return env, nil, fmt.Errorf("protected operator input %s is too short for safe redaction", key)
	}
	restore, err := installRedaction(patterns)
	if err != nil {
		return env, nil, err
	}
	env.secrets = append(env.secrets, patterns...)
	return env, restore, nil
}

func manualOperatorInputs(request tofuLockRequest, assignments []string) (operatorInputs, error) {
	result := operatorInputs{home: "", assignments: slices.Clone(assignments), publicNames: nil}
	home := environmentValue(request.env, "HOME")
	if home == "" {
		return result, errors.New("native execution requires the operator HOME")
	}
	canonical, err := filepath.EvalSymlinks(home)
	if err != nil {
		slog.Error("tofu.operator.home_failed", "err", err)
		return result, fmt.Errorf("resolve the operator HOME: %w", err)
	}
	result.home = canonical
	protection, err := tofuVariableProtection(request.workspaceDir)
	if err != nil {
		return result, err
	}
	existing := make(map[string]bool, len(assignments))
	for _, assignment := range assignments {
		name, _, _ := strings.Cut(assignment, "=")
		existing[name] = true
	}
	for _, name := range slices.Sorted(maps.Keys(protection)) {
		key := tofuVariablePrefix + name
		value, present := assignedEnvironmentValue(request.env, key)
		if !present || existing[key] {
			continue
		}
		result.assignments = append(result.assignments, key+"="+value)
		if !protection[name] {
			result.publicNames = append(result.publicNames, key)
		}
	}
	if value := environmentValue(request.env, "SSH_AUTH_SOCK"); value != "" && !existing["SSH_AUTH_SOCK"] {
		result.assignments = append(result.assignments, "SSH_AUTH_SOCK="+value)
		result.publicNames = append(result.publicNames, "SSH_AUTH_SOCK")
	}
	return result, nil
}

func manualSourceOperatorInputs(settings tofuSettings, request tofuLockRequest, assignments []string) (operatorInputs, error) {
	if settings.source != nil {
		request.workspaceDir = settings.source.path(request.workspaceDir)
	}
	return manualOperatorInputs(request, assignments)
}

func assignedEnvironmentValue(environment []string, name string) (string, bool) {
	value, present := "", false
	for _, assignment := range environment {
		key, candidate, found := strings.Cut(assignment, "=")
		if found && key == name {
			value, present = candidate, true
		}
	}
	return value, present
}

func tofuVariableProtection(directory string) (map[string]bool, error) {
	blocks, err := tofuModuleBlocks(directory, tofuVariableSchema)
	if err != nil {
		return nil, err
	}
	schema := &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "sensitive", Required: false}, {Name: "ephemeral", Required: false}}}
	result := make(map[string]bool, len(blocks))
	for _, block := range blocks {
		content, _, diagnostics := block.Body.PartialContent(schema)
		if diagnostics.HasErrors() {
			slog.Error("tofu.operator.variable_failed", "variable", block.Labels[0], "err", diagnostics.Error())
			return nil, fmt.Errorf("read variable protection flags: %w", diagnostics)
		}
		protected := false
		for _, attribute := range content.Attributes {
			value, diagnostics := attribute.Expr.Value(nil)
			if diagnostics.HasErrors() || value.IsNull() || !value.Type().Equals(cty.Bool) {
				return nil, fmt.Errorf("variable %s requires literal boolean protection flags", block.Labels[0])
			}
			protected = protected || value.RawEquals(cty.True)
		}
		result[block.Labels[0]] = protected
	}
	return result, nil
}
