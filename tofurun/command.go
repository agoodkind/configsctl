package tofurun

import (
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Run accepts these subcommands.
const (
	initCommand      = "init"
	workspaceCommand = "workspace select"
	planCommand      = "plan"
	showCommand      = "show"
	validateCommand  = "validate"
	fmtCommand       = "fmt"
	applyCommand     = "apply"
)

// Run checks these flags beyond the flag rules.
const (
	jsonFlag        = "json"
	lockFlag        = "lock"
	outFlag         = "out"
	parallelismFlag = "parallelism"
	trueValue       = "true"
	falseValue      = "false"
)

// flagRule lists the flags that one subcommand accepts. required maps a flag
// to its required value.
type flagRule struct {
	allowed  []string
	required map[string]string
}

var flagRules = map[string]flagRule{
	initCommand: {
		allowed:  []string{"input", "lockfile", "backend-config", "backend", "no-color", "reconfigure"},
		required: map[string]string{"input": falseValue, "lockfile": "readonly"},
	},
	workspaceCommand: {allowed: []string{"no-color"}, required: nil},
	planCommand: {
		allowed: []string{
			"input", "no-color", "detailed-exitcode", parallelismFlag, outFlag, "var-file", lockFlag, "lock-timeout",
		},
		required: map[string]string{"input": falseValue, "no-color": trueValue, "detailed-exitcode": trueValue},
	},
	showCommand:     {allowed: []string{"no-color", jsonFlag}, required: nil},
	validateCommand: {allowed: []string{"no-color", jsonFlag}, required: nil},
	fmtCommand: {
		allowed:  []string{"check", "diff", "recursive", "no-color", "list"},
		required: map[string]string{"check": trueValue},
	},
	applyCommand: {
		allowed:  []string{"input", parallelismFlag, "no-color", "lock-timeout"},
		required: map[string]string{"input": falseValue},
	},
}

// flag is one -name or -name=value argument. A flag without a value has the
// value "true".
type flag struct {
	name  string
	value string
}

// command is a parsed OpenTofu invocation.
type command struct {
	name        string
	flags       []flag
	positionals []string
	// positionalIndexes are the indexes of positionals in Request.Args.
	positionalIndexes []int
}

func parseCommand(args []string) (command, error) {
	parsed := command{name: "", flags: nil, positionals: nil, positionalIndexes: nil}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return parsed, rejection("tofurun.command_missing", "the arguments must start with an OpenTofu subcommand")
	}
	first := 1
	parsed.name = args[0]
	if args[0] == "workspace" {
		if len(args) < 2 || args[1] != "select" {
			return parsed, rejection("tofurun.command_unsupported", "the workspace command accepts only the select subcommand")
		}
		parsed.name, first = workspaceCommand, 2
	}
	if _, ok := flagRules[parsed.name]; !ok {
		return parsed, rejection("tofurun.command_unsupported", fmt.Sprintf("the OpenTofu subcommand %q is not supported", parsed.name))
	}
	for index := first; index < len(args); index++ {
		argument := args[index]
		if !strings.HasPrefix(argument, "-") {
			parsed.positionals = append(parsed.positionals, argument)
			parsed.positionalIndexes = append(parsed.positionalIndexes, index)
			continue
		}
		name, value, found := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		if !found {
			value = trueValue
		}
		parsed.flags = append(parsed.flags, flag{name: name, value: value})
	}
	return parsed, nil
}

// mutates reports whether the command changes infrastructure or state.
func (c command) mutates() bool {
	return c.name == applyCommand
}

// usesWorkspace reports whether the command reads the selected workspace
// state.
func (c command) usesWorkspace() bool {
	switch c.name {
	case planCommand, applyCommand:
		return true
	case showCommand:
		return len(c.positionals) == 0
	default:
		return false
	}
}

// value returns the last value of the named flag.
func (c command) value(name string) (string, bool) {
	value, found := "", false
	for _, item := range c.flags {
		if item.name == name {
			value, found = item.value, true
		}
	}
	return value, found
}

// check applies the flag and positional rules of the command.
func (c command) check(request Request, scoped bool) error {
	rule := flagRules[c.name]
	for _, item := range c.flags {
		if !slices.Contains(rule.allowed, item.name) {
			return rejection("tofurun.flag_refused", fmt.Sprintf("OpenTofu %s does not accept -%s here", c.name, item.name))
		}
	}
	for name, want := range rule.required {
		if got, _ := c.value(name); got != want {
			return rejection("tofurun.flag_missing", fmt.Sprintf("OpenTofu %s requires -%s=%s", c.name, name, want))
		}
	}
	switch c.name {
	case workspaceCommand:
		if len(c.positionals) != 1 || c.positionals[0] != request.Workspace {
			return rejection("tofurun.workspace_refused", fmt.Sprintf("workspace select requires exactly the workspace %q", request.Workspace))
		}
	case planCommand:
		if err := c.checkPlan(scoped); err != nil {
			return err
		}
	case showCommand:
		return c.checkShow(request.OutputMode)
	case applyCommand:
		if len(c.positionals) != 1 {
			return rejection("tofurun.saved_plan_missing", "OpenTofu apply requires exactly one saved plan")
		}
		if err := c.checkParallelism(); err != nil {
			return err
		}
	default:
		if len(c.positionals) != 0 {
			return rejection("tofurun.positional_refused", fmt.Sprintf("OpenTofu %s does not accept a positional argument", c.name))
		}
	}
	switch request.OutputMode {
	case RedactedProgress:
		return nil
	case PrivateJSON:
		return rejection("tofurun.output_refused", "private_json output requires show -json <saved-plan>")
	default:
		return rejection("tofurun.output_refused", fmt.Sprintf("the output mode %q is not supported", request.OutputMode))
	}
}

// checkPlan requires a positive -parallelism and an -out path, and it refuses
// -lock=false with a host lock scope.
func (c command) checkPlan(scoped bool) error {
	if lock, _ := c.value(lockFlag); scoped && lock == falseValue {
		return rejection("tofurun.lock_refused", "a plan with a host lock scope requires backend locking")
	}
	if len(c.positionals) != 0 {
		return rejection("tofurun.positional_refused", "OpenTofu plan does not accept a positional argument")
	}
	if out, _ := c.value(outFlag); out == "" || out == trueValue {
		return rejection("tofurun.flag_missing", "OpenTofu plan requires -out=<private-plan>")
	}
	return c.checkParallelism()
}

// checkParallelism requires -parallelism with a positive integer.
func (c command) checkParallelism() error {
	text, _ := c.value(parallelismFlag)
	parallelism, err := strconv.Atoi(text)
	if err != nil || parallelism < 1 {
		return rejection("tofurun.flag_missing", fmt.Sprintf("OpenTofu %s requires -parallelism=N with a positive integer N", c.name))
	}
	return nil
}

// savedPlanOutput returns the -out path of a plan, or an empty string for
// another command.
func (c command) savedPlanOutput() string {
	if c.name != planCommand {
		return ""
	}
	out, _ := c.value(outFlag)
	return out
}

func (c command) checkShow(mode OutputMode) error {
	if len(c.positionals) > 1 {
		return rejection("tofurun.positional_refused", "OpenTofu show accepts at most one saved plan")
	}
	jsonValue, isJSON := c.value(jsonFlag)
	switch mode {
	case PrivateJSON:
		if jsonValue != trueValue || len(c.positionals) != 1 || len(c.flags) != 1 {
			return rejection("tofurun.output_refused", "private_json output requires show -json <saved-plan>")
		}
	case RedactedProgress:
		if isJSON {
			return rejection("tofurun.output_refused", "show -json requires private_json output")
		}
	default:
		return rejection("tofurun.output_refused", fmt.Sprintf("the output mode %q is not supported", mode))
	}
	return nil
}

// savedPlanIndex returns the index in Request.Args of the saved plan that
// show or apply reads, or -1 when the command does not read a saved plan.
func (c command) savedPlanIndex() int {
	if (c.name == showCommand || c.name == applyCommand) && len(c.positionalIndexes) == 1 {
		return c.positionalIndexes[0]
	}
	return -1
}

// printableArgument accepts printable ASCII only. Run passes the binary path
// and each argument to the child process as one argv entry. Run does not use
// a shell.
var printableArgument = regexp.MustCompile(`^[\x20-\x7E]+$`)

// printable returns validated copies of the binary path and the arguments.
func printable(binary string, args []string) (string, []string, error) {
	safeBinary := printableArgument.FindString(binary)
	if safeBinary != binary {
		return "", nil, rejection("tofurun.argument_refused", "Binary contains a character that is not printable ASCII")
	}
	safeArgs := make([]string, 0, len(args))
	for index, argument := range args {
		match := printableArgument.FindString(argument)
		if match != argument {
			return "", nil, rejection("tofurun.argument_refused", fmt.Sprintf("argument %d contains a character that is not printable ASCII", index))
		}
		safeArgs = append(safeArgs, match)
	}
	return safeBinary, safeArgs, nil
}

// rejection logs and returns an error wrapping ErrRejected.
func rejection(event, reason string) error {
	err := fmt.Errorf("%w: %s", ErrRejected, reason)
	slog.Error(event, "reason", reason, "err", err)
	return err
}
