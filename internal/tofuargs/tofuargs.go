// Package tofuargs classifies OpenTofu command line arguments. It finds the
// subcommand, reads boolean flags in every accepted spelling, and determines
// whether an invocation writes to a run log.
package tofuargs

import (
	"strconv"
	"strings"
)

// Command is the subcommand of an OpenTofu invocation.
type Command string

// These subcommands determine the output destination, locks, and credentials.
// Apply and destroy keep terminal output unless auto-approval is enabled.
// Other subcommands use the terminal.
const (
	Plan    Command = "plan"
	Refresh Command = "refresh"
	Apply   Command = "apply"
	Destroy Command = "destroy"
)

// Boolean flags from tofu apply -help and tofu plan -help may precede a saved
// plan without consuming its path.
var booleanFlags = map[string]bool{
	"auto-approve":           true,
	"compact-warnings":       true,
	"concise":                true,
	"consolidate-errors":     true,
	"consolidate-warnings":   true,
	"destroy":                true,
	"detailed-exitcode":      true,
	"input":                  true,
	"json":                   true,
	"lock":                   true,
	"no-color":               true,
	"refresh":                true,
	"refresh-only":           true,
	"show-sensitive":         true,
	"suppress-forget-errors": true,
}

// Subcommand returns the first argument that is not a flag. OpenTofu runs
// that subcommand.
func Subcommand(args []string) Command {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return Command(arg)
		}
	}
	return ""
}

// Streams reports whether an invocation writes progress to a run log. Plan and
// refresh return true. Apply and destroy return true when AutoApprove returns
// true and otherwise keep terminal output for approval prompts. Other
// subcommands return false.
func Streams(args []string) bool {
	switch Subcommand(args) {
	case Plan, Refresh:
		return true
	case Apply, Destroy:
		return AutoApprove(args)
	default:
		return false
	}
}

// AutoApprove reports whether the arguments skip the approval prompt. It
// accepts every spelling tofu does: one or two leading dashes, bare or with an
// explicit value.
func AutoApprove(args []string) bool {
	return BooleanArgument(args, "auto-approve", false)
}

// BooleanArgument returns the last valid value for name or defaultValue when
// absent. It accepts one or two dashes, treats a bare flag as true, parses
// values with [strconv.ParseBool], retains the current result for invalid values,
// and skips values of other flags that consume an argument.
func BooleanArgument(args []string, name string, defaultValue bool) bool {
	value := defaultValue
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if !strings.HasPrefix(argument, "-") {
			continue
		}
		flag, text, found := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		if flag != name {
			if ConsumesValue(argument) {
				index++
			}
			continue
		}
		if !found {
			value = true
			continue
		}
		parsed, err := strconv.ParseBool(text)
		if err == nil {
			value = parsed
		}
	}
	return value
}

// ConsumesValue reports whether arg is a flag without "=" that takes the next
// argument as its value. Every flag except a known boolean flag takes a value.
func ConsumesValue(arg string) bool {
	if !strings.HasPrefix(arg, "-") || strings.Contains(arg, "=") {
		return false
	}
	return !booleanFlags[strings.TrimLeft(arg, "-")]
}
