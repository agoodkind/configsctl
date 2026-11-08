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

// booleanFlags lists flags that ConsumesValue treats as boolean flags.
// These flags do not consume the next argument as a value.
var booleanFlags = map[string]bool{
	"auto-approve":           true,
	"compact-warnings":       true,
	"concise":                true,
	"consolidate-errors":     true,
	"consolidate-warnings":   true,
	"destroy":                true,
	"detailed-exitcode":      true,
	"ignore-remote-version":  true,
	"input":                  true,
	"json":                   true,
	"lock":                   true,
	"no-color":               true,
	"refresh":                true,
	"refresh-only":           true,
	"show-sensitive":         true,
	"suppress-forget-errors": true,
}

// Subcommand returns the first argument that does not start with a dash.
//
// The function returns an empty command when every argument starts with a
// dash or args is empty.
//
//   - Subcommand([]string{"-chdir=prod", "plan"}) returns "plan".
//   - Subcommand([]string{"-chdir", "prod", "plan"}) returns "prod".
func Subcommand(args []string) Command {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return Command(arg)
		}
	}
	return ""
}

// Streams reports whether an invocation writes progress to a run log.
//
// Plan and refresh return true. Apply and destroy return the result of
// AutoApprove, which keeps terminal output for approval prompts when false.
// Other subcommands return false.
//
//   - Streams([]string{"plan"}) returns true.
//   - Streams([]string{"apply"}) returns false.
//   - Streams([]string{"apply", "-auto-approve"}) returns true.
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

// AutoApprove reports whether the arguments select automatic approval.
//
// The function reads the auto-approve flag with BooleanArgument and uses
// false as the default. It accepts one or two leading dashes and accepts a
// bare flag or an explicit value.
//
//   - AutoApprove([]string{"-auto-approve"}) returns true.
//   - AutoApprove([]string{"--auto-approve=false"}) returns false.
func AutoApprove(args []string) bool {
	return BooleanArgument(args, "auto-approve", false)
}

// BooleanArgument returns the last valid value of a boolean flag.
//
// The function uses defaultValue until it finds a valid value. It accepts
// one or two leading dashes, treats a bare flag as true, and parses explicit
// values with [strconv.ParseBool]. Invalid values leave the current result
// unchanged. The function skips the next argument when another flag consumes
// a value.
//
//   - BooleanArgument([]string{"-input=false", "--input"}, "input", false) returns true.
//   - BooleanArgument([]string{"-var", "-input", "-input=false"}, "input", true) returns false.
//   - BooleanArgument([]string{"-input=invalid"}, "input", true) returns true.
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

// ConsumesValue reports whether arg consumes the next argument as its value.
//
// The function returns true when arg starts with "-" and contains no "=",
// unless the flag name matches a known boolean flag.
//
//   - ConsumesValue("-var") returns true.
//   - ConsumesValue("-var=name=value") returns false.
//   - ConsumesValue("-no-color") returns false.
func ConsumesValue(arg string) bool {
	if !strings.HasPrefix(arg, "-") || strings.Contains(arg, "=") {
		return false
	}
	return !booleanFlags[strings.TrimLeft(arg, "-")]
}
