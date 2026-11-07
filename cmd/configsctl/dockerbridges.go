package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"goodkind.io/configsctl/internal/dockerbridge"
	"goodkind.io/configsctl/internal/tofuargs"
	"goodkind.io/configsctl/internal/tofulock"
)

const (
	dockerBridgesOutput        = "docker_bridges"
	dockerHostsVariable        = "docker_hosts"
	dockerBridgeDirectoryGlob  = "configsctl-docker-*"
	tofuImportCommand          = "import"
	tofuVariableArgument       = "var"
	dockerHostsArgumentPattern = dockerHostsVariable + "="
)

// runWithDockerBridges starts the Docker bridges in directory, runs run with
// the bridge context and the TF_VAR_docker_hosts assignment, and stops the
// bridges after run returns. Without bridges it runs run with ctx.
func runWithDockerBridges(
	ctx context.Context, bridges map[string]dockerbridge.Bridge, directory string, environment []string, getOnly bool,
	run func(context.Context, []string) error,
) error {
	if len(bridges) == 0 {
		return run(ctx, nil)
	}
	session, bridgeContext, err := dockerbridge.Start(ctx, directory, bridges, environment, getOnly)
	if err != nil {
		slog.Error("tofu.docker_bridge.start_failed", "err", err)
		return fmt.Errorf("start the Docker bridges: %w", err)
	}
	runErr := run(bridgeContext, []string{session.Assignment()})
	if stopErr := session.Stop(); stopErr != nil {
		slog.Error("tofu.docker_bridge.stop_failed", "err", stopErr)
		return errors.Join(runErr, fmt.Errorf("stop the Docker bridges: %w", stopErr))
	}
	return runErr
}

// runInDockerBridgeDirectory creates a private directory for the bridge
// sockets of a command that does not have a private run directory.
func runInDockerBridgeDirectory(
	ctx context.Context, bridges map[string]dockerbridge.Bridge, environment []string, getOnly bool,
	run func(context.Context, []string) error,
) error {
	if len(bridges) == 0 {
		return run(ctx, nil)
	}
	directory, err := os.MkdirTemp("", dockerBridgeDirectoryGlob)
	if err != nil {
		slog.Error("tofu.docker_bridge.directory_failed", "err", err)
		return fmt.Errorf("create the Docker bridge directory: %w", err)
	}
	defer removePrivateDirectory(directory)
	if err := makePrivateDirectory(directory); err != nil {
		return err
	}
	return runWithDockerBridges(ctx, bridges, directory, environment, getOnly, run)
}

// dockerBridgeAccess reports whether an OpenTofu subcommand starts the Docker
// bridges and whether those bridges accept only reads.
func dockerBridgeAccess(args []string) (bridged, getOnly bool) {
	if tofuCommandHelp(args) {
		return false, false
	}
	switch tofuargs.Subcommand(args) {
	case tofuargs.Plan, tofuImportCommand:
		return true, true
	case tofuargs.Apply, tofuargs.Destroy:
		return true, false
	case tofuargs.Refresh:
		return false, false
	default:
		return false, false
	}
}

// requireDockerBridgeInputs refuses an operator docker_hosts value and a
// missing token secret before the command starts OpenTofu.
func requireDockerBridgeInputs(bridges map[string]dockerbridge.Bridge, args, environment []string) error {
	if len(bridges) == 0 {
		return nil
	}
	aliases := slices.Sorted(maps.Keys(bridges))
	var err error
	if _, present := assignedEnvironmentValue(environment, dockerbridge.HostsVariable); present {
		err = fmt.Errorf("%s conflicts with the Docker bridges for %v; unset it because configsctl sets it for each command",
			dockerbridge.HostsVariable, aliases)
	} else if assignsDockerHosts(args) {
		err = fmt.Errorf("-var %s conflicts with the Docker bridges for %v; remove it because configsctl sets it for each command",
			dockerHostsVariable, aliases)
	} else if bridged, _ := dockerBridgeAccess(args); bridged {
		err = requireDockerBridgeTokens(bridges, aliases, environment)
	}
	if err != nil {
		slog.Error("tofu.docker_bridge.inputs_refused", "aliases", aliases, "err", err)
	}
	return err
}

func requireDockerBridgeTokens(bridges map[string]dockerbridge.Bridge, aliases, environment []string) error {
	for _, alias := range aliases {
		name := tofuVariablePrefix + bridges[alias].TokenVariable
		if environmentValue(environment, name) == "" {
			return fmt.Errorf("the Docker bridge %s requires the token secret %s; add the vault key %s",
				alias, name, bridges[alias].TokenVariable)
		}
	}
	return nil
}

func assignsDockerHosts(args []string) bool {
	for index, argument := range args {
		if argument == "--" {
			return false
		}
		name, value, inline := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		if !strings.HasPrefix(argument, "-") || name != tofuVariableArgument {
			continue
		}
		if !inline && index+1 < len(args) {
			value = args[index+1]
		}
		if strings.HasPrefix(value, dockerHostsArgumentPattern) {
			return true
		}
	}
	return false
}

// decodeDockerBridges reads the docker_bridges projection output. It returns
// nil when the output is absent, null, or empty.
func decodeDockerBridges(selector string, raw json.RawMessage, targets tofulock.Targets) (map[string]dockerbridge.Bridge, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var bridges map[string]dockerbridge.Bridge
	if err := decoder.Decode(&bridges); err != nil {
		slog.Error("tofu.configuration.docker_bridges_invalid", "workspace", selector, "err", err)
		return nil, fmt.Errorf("decode the docker_bridges output for %s: %w", selector, err)
	}
	if len(bridges) == 0 {
		return nil, nil
	}
	for _, alias := range slices.Sorted(maps.Keys(bridges)) {
		bridge := bridges[alias]
		if err := dockerbridge.ValidateAlias(alias); err != nil {
			return nil, fmt.Errorf("native configuration for %s: %w", selector, err)
		}
		if err := bridge.Validate(); err != nil {
			return nil, fmt.Errorf("native configuration for %s: docker_bridges.%s: %w", selector, alias, err)
		}
		if _, owned := targets.GuestHosts[bridge.GuestKey()]; !owned {
			return nil, fmt.Errorf("native configuration for %s: docker_bridges.%s uses guest %s, which guest_hosts does not declare",
				selector, alias, bridge.GuestKey())
		}
	}
	return bridges, nil
}

// requireDockerBridgeVariables requires the ephemeral docker_hosts variable
// and the token variable of each bridge in the workspace module.
func requireDockerBridgeVariables(directory string, bridges map[string]dockerbridge.Bridge) error {
	if len(bridges) == 0 {
		return nil
	}
	ephemeral, err := tofuVariableFlags(directory, []string{"ephemeral"})
	if err != nil {
		return err
	}
	if !ephemeral[dockerHostsVariable] {
		return fmt.Errorf("workspace %s declares Docker bridges and must declare the ephemeral variable %s",
			directory, dockerHostsVariable)
	}
	for _, alias := range slices.Sorted(maps.Keys(bridges)) {
		token := bridges[alias].TokenVariable
		if _, declared := ephemeral[token]; !declared {
			return fmt.Errorf("workspace %s must declare the variable %s for the token of the Docker bridge %s",
				directory, token, alias)
		}
	}
	return nil
}

// tofuVariableFlags reports, for each variable in directory, whether any of
// the named literal boolean attributes is true.
func tofuVariableFlags(directory string, names []string) (map[string]bool, error) {
	blocks, err := tofuModuleBlocks(directory, tofuVariableSchema)
	if err != nil {
		return nil, err
	}
	attributes := make([]hcl.AttributeSchema, 0, len(names))
	for _, name := range names {
		attributes = append(attributes, hcl.AttributeSchema{Name: name, Required: false})
	}
	schema := &hcl.BodySchema{Attributes: attributes}
	result := make(map[string]bool, len(blocks))
	for _, block := range blocks {
		content, _, diagnostics := block.Body.PartialContent(schema)
		if diagnostics.HasErrors() {
			slog.Error("tofu.operator.variable_failed", "variable", block.Labels[0], "err", diagnostics.Error())
			return nil, fmt.Errorf("read variable protection flags: %w", diagnostics)
		}
		enabled := false
		for _, attribute := range content.Attributes {
			value, diagnostics := attribute.Expr.Value(nil)
			if diagnostics.HasErrors() || value.IsNull() || !value.Type().Equals(cty.Bool) {
				return nil, fmt.Errorf("variable %s requires literal boolean protection flags", block.Labels[0])
			}
			enabled = enabled || value.RawEquals(cty.True)
		}
		result[block.Labels[0]] = enabled
	}
	return result, nil
}
