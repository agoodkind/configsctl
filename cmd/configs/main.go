// Command configs is the control tool for the configs repository. It runs
// Ansible deploys, manages vault secrets, and lints Ansible input variables.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"goodkind.io/configs/internal/baseline"
	"goodkind.io/configs/internal/lint"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("configs failed", "err", err)
		os.Exit(1)
	}
}

// handlers dispatches a subcommand name to its implementation.
var handlers = map[string]func([]string) error{
	"lint":     runLint,
	"baseline": runBaseline,
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: configs <command>")
	}
	handler, ok := handlers[args[0]]
	if !ok {
		return fmt.Errorf("unknown command: %q", args[0])
	}
	return handler(args[1:])
}

func runLint(paths []string) error {
	if len(paths) == 0 {
		paths = lint.Discover(".")
	}
	newFindings, blocked, bypassToken := lint.Gate(paths)
	for _, line := range newFindings {
		fmt.Println(line)
	}
	if bypassToken != "" {
		fmt.Printf("*** lint non-blocking via BYPASS_LINT=%s\n", bypassToken)
	}
	if blocked {
		return fmt.Errorf("%d input-default violation(s)", len(newFindings))
	}
	return nil
}

func runBaseline(args []string) error {
	value := modeFlag(args)
	mode, err := baseline.ParseMode(value)
	if err != nil {
		return fmt.Errorf("invalid baseline mode %q", value)
	}
	if lint.WriteBaseline(lint.Discover("."), mode) {
		fmt.Println("baseline updated")
	}
	return nil
}

// modeFlag reads the value of a --mode flag, accepting --mode=value and
// --mode value forms. It returns empty when the flag is absent.
func modeFlag(args []string) string {
	for i, arg := range args {
		if value, ok := strings.CutPrefix(arg, "--mode="); ok {
			return value
		}
		if arg == "--mode" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
