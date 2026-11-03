package main

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	"goodkind.io/configsctl/internal/tofuargs"
	"goodkind.io/configsctl/internal/tofulock"
)

const parallelismFlagName = "parallelism"

// parallelismCommands accept the -parallelism flag.
var parallelismCommands = []tofuargs.Command{tofuargs.Plan, tofuargs.Apply, tofuargs.Destroy, tofuargs.Refresh}

// backendLockedCommands write state. A workspace policy refuses -lock=false for them.
var backendLockedCommands = []tofuargs.Command{tofuargs.Apply, tofuargs.Destroy, tofuargs.Refresh}

// unguardedStateCommands write state without the explicit plan and apply route.
// A workspace with required_hosts refuses them.
var unguardedStateCommands = []tofuargs.Command{
	tofuargs.Destroy, tofuargs.Refresh, "import", "taint", "untaint", "force-unlock",
}

// unguardedStateSubcommands are the "tofu state" subcommands that write state.
var unguardedStateSubcommands = []string{"rm", "remove", "mv", "move", "push", "replace-provider"}

// hasWorkspacePolicy reports whether the workspace requires host locks or limits parallelism.
func hasWorkspacePolicy(targets tofulock.Targets) bool {
	return len(targets.RequiredHosts) > 0 || targets.MaxParallelism != nil
}

// preparePolicyInvocation expands the OpenTofu arguments and environment, then
// applies the lock and parallelism policy of the selected workspace.
func preparePolicyInvocation(settings tofuSettings, workspaceDir, passwordFile string, args []string) (safeArgs, childEnv []string, err error) {
	safeArgs, childEnv, err = prepareTofuInvocation(settings, workspaceDir, passwordFile, args)
	if err != nil {
		return nil, nil, err
	}
	workspace := tofuWorkspaceKey(workspaceDir)
	safeArgs, err = applyWorkspacePolicy(workspace, settings.LockTargets[workspace], safeArgs)
	if err != nil {
		return nil, nil, err
	}
	return safeArgs, childEnv, nil
}

// applyWorkspacePolicy enforces the lock and parallelism policy of a workspace on
// the expanded OpenTofu arguments, which already include TF_CLI_ARGS values. It
// returns the arguments with the configured -parallelism when none is present.
// A workspace without a policy keeps its arguments.
func applyWorkspacePolicy(workspace string, targets tofulock.Targets, args []string) ([]string, error) {
	if !hasWorkspacePolicy(targets) {
		return args, nil
	}
	if tofuargs.BooleanArgument(args, "help", false) || tofuargs.BooleanArgument(args, "h", false) {
		return args, nil
	}
	command := tofuargs.Subcommand(args)
	if err := requirePolicyCommand(workspace, targets, command, args); err != nil {
		return nil, err
	}
	if slices.Contains(backendLockedCommands, command) && !tofuargs.BooleanArgument(args, "lock", true) {
		err := fmt.Errorf("workspace %s has a lock policy, and %s cannot run with -lock=false", workspace, command)
		slog.Error("tofu.policy.lock_disabled", "workspace", workspace, "command", string(command), "err", err)
		return nil, err
	}
	if targets.MaxParallelism == nil || !slices.Contains(parallelismCommands, command) {
		return args, nil
	}
	return enforceParallelism(workspace, *targets.MaxParallelism, args)
}

// requirePolicyCommand refuses a state-writing command that skips the host
// locks of a workspace with required_hosts.
func requirePolicyCommand(workspace string, targets tofulock.Targets, command tofuargs.Command, args []string) error {
	if len(targets.RequiredHosts) == 0 {
		return nil
	}
	if !slices.Contains(unguardedStateCommands, command) && !isUnguardedStateSubcommand(command, args) {
		return nil
	}
	err := fmt.Errorf("workspace %s requires locks on hosts %v, but %s writes state without them; use plan and apply",
		workspace, targets.RequiredHosts, command)
	slog.Error("tofu.policy.unguarded_command", "workspace", workspace, "command", string(command),
		"required_hosts", targets.RequiredHosts, "err", err)
	return err
}

func isUnguardedStateSubcommand(command tofuargs.Command, args []string) bool {
	if command != "state" {
		return false
	}
	index := tofuargs.SubcommandIndex(args)
	if index < 0 || index+1 >= len(args) {
		return false
	}
	return slices.Contains(unguardedStateSubcommands, args[index+1])
}

func enforceParallelism(workspace string, limit int, args []string) ([]string, error) {
	values, err := tofuargs.IntegerArguments(args, parallelismFlagName)
	if err != nil {
		slog.Error("tofu.policy.parallelism_invalid", "workspace", workspace, "err", err)
		return nil, fmt.Errorf("workspace %s: %w", workspace, err)
	}
	for _, value := range values {
		if err := checkParallelismLimit(workspace, value, limit); err != nil {
			return nil, err
		}
	}
	if len(values) > 0 {
		return args, nil
	}
	index := tofuargs.SubcommandIndex(args)
	flag := "-" + parallelismFlagName + "=" + strconv.Itoa(limit)
	return slices.Insert(slices.Clone(args), index+1, flag), nil
}

// checkParallelismLimit counts every -parallelism value, from the command line
// and from TF_CLI_ARGS, because OpenTofu would apply the last one.
func checkParallelismLimit(workspace string, value, limit int) error {
	if value >= 1 && value <= limit {
		return nil
	}
	err := fmt.Errorf("workspace %s: -parallelism=%d is outside 1..%d, the max_parallelism of the workspace",
		workspace, value, limit)
	slog.Error("tofu.policy.parallelism_refused", "workspace", workspace, "value", value, "max_parallelism", limit, "err", err)
	return err
}
