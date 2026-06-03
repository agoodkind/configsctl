// Command configs is the control tool for the configs repository. It runs
// Ansible deploys, manages vault secrets, and lints Ansible input variables.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"goodkind.io/configs/internal/lint"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("configs failed", "err", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: configs <command>")
	}
	switch args[0] {
	case "lint":
		return runLint(args[1:])
	default:
		return fmt.Errorf("unknown command: %q", args[0])
	}
}

func runLint(paths []string) error {
	findings := lint.Run(paths)
	for _, finding := range findings {
		fmt.Printf("%s:%d: banned default or presence check: %s on %s\n",
			finding.File, finding.Line, finding.Kind, finding.Root)
	}
	if len(findings) > 0 {
		return fmt.Errorf("%d input-default violation(s)", len(findings))
	}
	return nil
}
