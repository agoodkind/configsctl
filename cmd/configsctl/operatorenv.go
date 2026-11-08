package main

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"goodkind.io/configsctl/internal/dockerbridge"
	"goodkind.io/configsctl/internal/redact"
)

type operatorInputs struct {
	home        string
	assignments []string
	publicNames []string
}

func registerOperatorRedaction(env cmdEnv, settings tofuSettings, workspace string, args, environment []string) (cmdEnv, func(), error) {
	patterns := dockerBridgeTokenPatterns(settings.dockerBridges, args, environment)
	if tofuMutates(args) && !strictExplicitSettings(settings, settings.LockTargets[tofuWorkspaceKey(workspace)]) {
		protected, err := protectedOperatorPatterns(settings, workspace, environment)
		if err != nil {
			return env, nil, err
		}
		patterns = append(patterns, protected...)
	}
	if len(patterns) == 0 {
		return env, func() {}, nil
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

func protectedOperatorPatterns(settings tofuSettings, workspace string, environment []string) ([]redact.Pattern, error) {
	inputWorkspace := workspace
	if settings.source != nil {
		inputWorkspace = settings.source.path(workspace)
	}
	protection, err := tofuVariableProtection(inputWorkspace)
	if err != nil {
		return nil, err
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
	return patterns, nil
}

func rawVariableAssignments(args []string) []string {
	var assignments []string
	for index, argument := range args {
		name, value, inline := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		if !strings.HasPrefix(argument, "-") || name != tofuVariableArgument {
			continue
		}
		if inline {
			assignments = append(assignments, value)
		} else if index+1 < len(args) {
			assignments = append(assignments, args[index+1])
		}
	}
	return assignments
}

func dockerBridgeTokenPatterns(bridges map[string]dockerbridge.Bridge, args, environment []string) []redact.Pattern {
	var patterns []redact.Pattern
	assignments := rawVariableAssignments(args)
	for _, alias := range slices.Sorted(maps.Keys(bridges)) {
		token := bridges[alias].TokenVariable
		key := tofuVariablePrefix + token
		if value := environmentValue(environment, key); value != "" {
			patterns = append(patterns, redact.Pattern{Label: key, Value: []byte(value)})
		}
		for _, assignment := range assignments {
			if value, matched := strings.CutPrefix(assignment, token+"="); matched && value != "" {
				patterns = append(patterns, redact.Pattern{Label: key, Value: []byte(value)})
			}
		}
	}
	return patterns
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
	return tofuVariableFlags(directory, []string{"sensitive", "ephemeral"})
}
