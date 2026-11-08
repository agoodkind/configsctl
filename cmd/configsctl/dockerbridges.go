package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"goodkind.io/configsctl/internal/dockerbridge"
	"goodkind.io/configsctl/internal/tofuargs"
	"goodkind.io/configsctl/internal/tofulock"
)

const (
	dockerBridgeDirectoryGlob  = "configsctl-docker-*"
	tofuImportCommand          = "import"
	tofuVariableArgument       = "var"
	dockerHostsArgumentPattern = dockerbridge.HostsVariable + "="
	argumentTerminator         = "--"
	overrideStem               = "override"
	overrideSuffix             = "_override"
)

// tofuSourceExtensions lists the source file extensions that OpenTofu loads,
// with the longest extension first.
var tofuSourceExtensions = []string{".tofu.json", ".tf.json", ".tofu", ".tf"}

// dockerBridgeRedirections lists the OpenTofu options that select a
// configuration other than the workspace that configsctl validates.
var dockerBridgeRedirections = []string{"chdir", "config"}

// runWithDockerBridges starts the Docker bridges in directory and runs run
// with the bridge context and a function that adds the docker_hosts argument
// to OpenTofu arguments. It stops the bridges after run returns. Without
// bridges, run receives ctx and a function that copies the arguments.
func runWithDockerBridges(
	ctx context.Context, bridges map[string]dockerbridge.Bridge, directory string, environment []string,
	writable map[string]bool, run func(context.Context, func([]string) []string) error,
) error {
	if len(bridges) == 0 {
		return run(ctx, slices.Clone[[]string])
	}
	session, bridgeContext, err := dockerbridge.Start(ctx, directory, bridges, dockerBridgeTokens(bridges, environment), writable)
	if err != nil {
		slog.Error("tofu.docker_bridge.start_failed", "err", err)
		return fmt.Errorf("start the Docker bridges: %w", err)
	}
	runErr := run(bridgeContext, func(args []string) []string { return canonicalTofuArguments(args, session.Argument()) })
	if stopErr := session.Stop(); stopErr != nil {
		slog.Error("tofu.docker_bridge.stop_failed", "err", stopErr)
		return errors.Join(runErr, fmt.Errorf("stop the Docker bridges: %w", stopErr))
	}
	return runErr
}

// runInDockerBridgeDirectory creates a private directory for the read-only
// bridge sockets of a command that does not have a private run directory.
func runInDockerBridgeDirectory(
	ctx context.Context, bridges map[string]dockerbridge.Bridge, environment []string,
	run func(context.Context, func([]string) []string) error,
) error {
	if len(bridges) == 0 {
		return run(ctx, slices.Clone[[]string])
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
	return runWithDockerBridges(ctx, bridges, directory, environment, nil, run)
}

// dockerBridgeAccess reports whether an OpenTofu subcommand starts Docker
// bridges and whether those bridges allow only GET and HEAD requests.
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

// writableDockerBridges marks an alias writable when lockedHosts contains the
// guest_hosts host of its guest. The apply step checks lockedHosts against its
// host lock scope.
func writableDockerBridges(bridges map[string]dockerbridge.Bridge, guestHosts map[string]string, lockedHosts []string) map[string]bool {
	writable := make(map[string]bool, len(bridges))
	for alias, bridge := range bridges {
		host, owned := guestHosts[bridge.GuestKey()]
		writable[alias] = owned && slices.Contains(lockedHosts, host)
	}
	return writable
}

// requireDockerBridgeInputs rejects an operator docker_hosts value and an
// option that selects another configuration when bridges exist. For commands
// that start bridges, it also requires each token secret.
func requireDockerBridgeInputs(bridges map[string]dockerbridge.Bridge, args, environment []string) error {
	if len(bridges) == 0 {
		return nil
	}
	aliases := slices.Sorted(maps.Keys(bridges))
	var err error
	environmentName := tofuVariablePrefix + dockerbridge.HostsVariable
	if _, present := assignedEnvironmentValue(environment, environmentName); present {
		err = fmt.Errorf("%s conflicts with the Docker bridges for %v; unset it because configsctl sets it for each command",
			environmentName, aliases)
	} else if assignsDockerHosts(args) {
		err = fmt.Errorf("-var %s conflicts with the Docker bridges for %v; remove it because configsctl sets it for each command",
			dockerbridge.HostsVariable, aliases)
	} else if option := redirectionOption(args); option != "" {
		err = fmt.Errorf("-%s selects another configuration, and the Docker bridges for %v run only for the validated workspace; remove it",
			option, aliases)
	} else if bridged, _ := dockerBridgeAccess(args); bridged {
		err = requireDockerBridgeTokens(bridges, aliases, environment)
	}
	if err != nil {
		slog.Error("tofu.docker_bridge.inputs_refused", "aliases", aliases, "err", err)
	}
	return err
}

func dockerBridgeTokens(bridges map[string]dockerbridge.Bridge, environment []string) map[string]string {
	tokens := make(map[string]string, len(bridges))
	for alias, bridge := range bridges {
		tokens[alias] = environmentValue(environment, tofuVariablePrefix+bridge.TokenVariable)
	}
	return tokens
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

func optionName(argument string) (string, bool) {
	if !strings.HasPrefix(argument, "-") {
		return "", false
	}
	name, _, _ := strings.Cut(strings.TrimLeft(argument, "-"), "=")
	return name, true
}

type tofuOption struct {
	name  string
	value string
}

func tofuOptions(args []string) []tofuOption {
	var options []tofuOption
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == argumentTerminator {
			break
		}
		name, option := optionName(argument)
		if !option {
			continue
		}
		_, value, inline := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		if !inline && tofuargs.ConsumesValue(argument) && index+1 < len(args) {
			index++
			value = args[index]
		}
		options = append(options, tofuOption{name: name, value: value})
	}
	return options
}

func redirectionOption(args []string) string {
	for _, option := range tofuOptions(args) {
		if slices.Contains(dockerBridgeRedirections, option.name) {
			return option.name
		}
	}
	return ""
}

func canonicalTofuArguments(args []string, generated ...string) []string {
	subcommand := slices.IndexFunc(args, func(value string) bool { return !strings.HasPrefix(value, "-") })
	if subcommand < 0 {
		return slices.Concat(args, generated)
	}
	var options, positionals, terminated []string
	for index := subcommand + 1; index < len(args); index++ {
		argument := args[index]
		if argument == argumentTerminator {
			terminated = args[index:]
			break
		}
		if !strings.HasPrefix(argument, "-") {
			positionals = append(positionals, argument)
			continue
		}
		options = append(options, argument)
		if tofuargs.ConsumesValue(argument) && index+1 < len(args) {
			index++
			options = append(options, args[index])
		}
	}
	if terminated == nil {
		return slices.Concat(args[:subcommand+1], options, generated, positionals)
	}
	return slices.Concat(args[:subcommand+1], options, generated, terminated[:1], positionals, terminated[1:])
}

func variableAssignments(args []string) []string {
	var assignments []string
	for _, option := range tofuOptions(args) {
		if option.name == tofuVariableArgument {
			assignments = append(assignments, option.value)
		}
	}
	return assignments
}

func assignsDockerHosts(args []string) bool {
	return slices.ContainsFunc(variableAssignments(args), func(assignment string) bool {
		return strings.HasPrefix(assignment, dockerHostsArgumentPattern)
	})
}

func validateDockerBridges(selector string, bridges map[string]dockerbridge.Bridge, targets tofulock.Targets) error {
	for _, alias := range slices.Sorted(maps.Keys(bridges)) {
		bridge := bridges[alias]
		if err := dockerbridge.ValidateAlias(alias); err != nil {
			return fmt.Errorf("native configuration for %s: %w", selector, err)
		}
		if err := bridge.Validate(); err != nil {
			slog.Error("tofu.configuration.docker_bridge_invalid", "workspace", selector, "alias", alias, "err", err)
			return fmt.Errorf("native configuration for %s: docker_bridges.%s: %w", selector, alias, err)
		}
		if _, owned := targets.GuestHosts[bridge.GuestKey()]; !owned {
			return fmt.Errorf("native configuration for %s: docker_bridges.%s uses guest %s, which guest_hosts does not declare",
				selector, alias, bridge.GuestKey())
		}
	}
	return nil
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
	if !ephemeral[dockerbridge.HostsVariable] {
		return fmt.Errorf("workspace %s declares Docker bridges and must declare the ephemeral variable %s",
			directory, dockerbridge.HostsVariable)
	}
	for _, alias := range slices.Sorted(maps.Keys(bridges)) {
		token := bridges[alias].TokenVariable
		if !ephemeral[token] {
			return fmt.Errorf("workspace %s must declare the variable %s with ephemeral = true for the token of the Docker bridge %s",
				directory, token, alias)
		}
	}
	return nil
}

// tofuOverrideFile reports whether path has an OpenTofu override file name.
func tofuOverrideFile(path string) bool {
	name := filepath.Base(path)
	for _, extension := range tofuSourceExtensions {
		if stem, matched := strings.CutSuffix(name, extension); matched {
			return stem == overrideStem || strings.HasSuffix(stem, overrideSuffix)
		}
	}
	return false
}

// tofuVariableFlags reports, for each variable in directory, whether any
// literal boolean attribute in names is true. An attribute in an override
// file replaces the primary value, and override files apply in name order.
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
	primary := make(map[string]map[string]bool, len(blocks))
	var overrides hcl.Blocks
	for _, block := range blocks {
		if tofuOverrideFile(block.DefRange.Filename) {
			overrides = append(overrides, block)
			continue
		}
		values, err := variableFlagValues(block, schema)
		if err != nil {
			return nil, err
		}
		primary[block.Labels[0]] = values
	}
	slices.SortStableFunc(overrides, func(first, second *hcl.Block) int {
		return strings.Compare(filepath.Base(first.DefRange.Filename), filepath.Base(second.DefRange.Filename))
	})
	for _, block := range overrides {
		values, err := variableFlagValues(block, schema)
		if err != nil {
			return nil, err
		}
		if base, declared := primary[block.Labels[0]]; declared {
			maps.Copy(base, values)
		}
	}
	result := make(map[string]bool, len(primary))
	for name, values := range primary {
		result[name] = slices.Contains(slices.Collect(maps.Values(values)), true)
	}
	return result, nil
}

func variableFlagValues(block *hcl.Block, schema *hcl.BodySchema) (map[string]bool, error) {
	content, _, diagnostics := block.Body.PartialContent(schema)
	if diagnostics.HasErrors() {
		slog.Error("tofu.operator.variable_failed", "variable", block.Labels[0], "err", diagnostics.Error())
		return nil, fmt.Errorf("read variable protection flags: %w", diagnostics)
	}
	values := make(map[string]bool, len(content.Attributes))
	for name, attribute := range content.Attributes {
		value, diagnostics := attribute.Expr.Value(nil)
		if diagnostics.HasErrors() || value.IsNull() || !value.Type().Equals(cty.Bool) {
			return nil, fmt.Errorf("variable %s requires literal boolean protection flags", block.Labels[0])
		}
		values[name] = value.RawEquals(cty.True)
	}
	return values, nil
}
