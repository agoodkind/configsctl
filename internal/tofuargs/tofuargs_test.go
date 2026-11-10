package tofuargs_test

import (
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/tofuargs"
)

// TestTofuStreams checks which OpenTofu invocations write to a run log.
// Approval prompts require the terminal for operator input.
func TestTofuStreams(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "plan streams", args: []string{"plan"}, want: true},
		{name: "plan with flags streams", args: []string{"-chdir=x", "plan", "-detailed-exitcode"}, want: true},
		{name: "refresh streams", args: []string{"refresh"}, want: true},
		{name: "apply prompts", args: []string{"apply"}, want: false},
		{name: "apply auto-approve streams", args: []string{"apply", "-auto-approve"}, want: true},
		{name: "apply double dash auto-approve streams", args: []string{"apply", "--auto-approve"}, want: true},
		{name: "apply auto-approve with value streams", args: []string{"apply", "-auto-approve=true"}, want: true},
		{name: "destroy prompts", args: []string{"destroy"}, want: false},
		{name: "destroy auto-approve streams", args: []string{"destroy", "-auto-approve"}, want: true},
		{name: "console keeps the terminal", args: []string{"console"}, want: false},
		{name: "init keeps the terminal", args: []string{"init"}, want: false},
		{name: "output keeps the terminal", args: []string{"output", "-json"}, want: false},
		{name: "unknown subcommand keeps the terminal", args: []string{"whatever"}, want: false},
		{name: "no subcommand keeps the terminal", args: []string{"-help"}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tofuargs.Streams(tc.args); got != tc.want {
				t.Fatalf("Streams(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestTofuSubcommand checks that the run log filename uses the subcommand
// instead of a leading global flag.
func TestTofuSubcommand(t *testing.T) {
	tests := []struct {
		args []string
		want tofuargs.Command
	}{
		{args: []string{"plan"}, want: tofuargs.Plan},
		{args: []string{"-chdir=opentofu", "apply"}, want: tofuargs.Apply},
		{args: []string{"-help"}, want: ""},
		{args: nil, want: ""},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			if got := tofuargs.Subcommand(tc.args); got != tc.want {
				t.Fatalf("Subcommand(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
