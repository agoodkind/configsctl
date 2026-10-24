package main

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/mattn/go-shellwords"
)

const tofuCLIArgs = "TF_CLI_ARGS"

// These command paths match the OpenTofu v1.12.6 command registry.
var tofuNestedCommands = map[string][]string{
	"env":       {"list", "select", "new", "delete"},
	"metadata":  {"functions"},
	"providers": {"lock", "mirror", "schema"},
	"state":     {"list", "ls", "rm", "remove", "mv", "move", "pull", "push", "show", "replace-provider"},
	"workspace": {"list", "select", "show", "new", "delete"},
}

func tofuInvocation(settings tofuSettings, workspace, passwordFile string, args []string) ([]string, []string, error) {
	secrets, err := tofuSecretEnv(settings, workspace, defaultVaultFile, passwordFile)
	if err != nil {
		return nil, nil, err
	}
	return expandTofuEnvironment(args, append(os.Environ(), secrets...))
}

// Expand native environment arguments once before selecting approval and locks.
func expandTofuEnvironment(args, environment []string) ([]string, []string, error) {
	command, insertion := tofuEnvironmentCommand(args)
	global, err := parseTofuEnvironment(tofuCLIArgs, environment)
	if err != nil {
		return nil, nil, err
	}
	suffix := strings.NewReplacer("-", "_", " ", "_").Replace(command)
	specific, err := parseTofuEnvironment(tofuCLIArgs+"_"+suffix, environment)
	if err != nil {
		return nil, nil, err
	}
	merged := slices.Concat(args[:insertion], specific, global, args[insertion:])
	clean := make([]string, 0, len(environment))
	for _, assignment := range environment {
		variable, _, _ := strings.Cut(assignment, "=")
		switch variable {
		case tofuCLIArgs:
			continue
		}
		if strings.HasPrefix(variable, tofuCLIArgs+"_") {
			continue
		}
		clean = append(clean, assignment)
	}
	return merged, clean, nil
}

func parseTofuEnvironment(name string, environment []string) ([]string, error) {
	parser := shellwords.NewParser()
	parser.ParseEnv = false
	parser.ParseBacktick = false
	args, err := parser.Parse(environmentValue(environment, name))
	if err != nil {
		return nil, fmt.Errorf("parse OpenTofu arguments from %s; check quoting and escaping", name)
	}
	safe, err := sanitizeTofuArgs(args)
	if err != nil {
		return nil, fmt.Errorf("OpenTofu arguments from %s contain non-printable characters", name)
	}
	return safe, nil
}

func tofuEnvironmentCommand(args []string) (string, int) {
	for index, argument := range args {
		if strings.HasPrefix(argument, "-") {
			continue
		}
		if index+1 < len(args) && slices.Contains(tofuNestedCommands[argument], args[index+1]) {
			return argument + " " + args[index+1], index + 2
		}
		return argument, index + 1
	}
	return "", 0
}
