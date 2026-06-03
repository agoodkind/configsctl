// Command configs is the control tool for the configs repository. It runs
// Ansible deploys, manages vault secrets, and lints Ansible input variables.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	return fmt.Errorf("unknown command: %q", args[0])
}
