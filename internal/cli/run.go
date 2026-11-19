// Package cli dispatches configsctl commands that run outside vault setup.
package cli

import (
	"errors"

	"goodkind.io/configsctl/internal/hclsources"
	"goodkind.io/configsctl/retirement"
)

// Run dispatches the command from the process argument list.
//
// The configuration commands use the existing operator command handler.
// Pass the full argument list, including the program name. Run returns
// the matching error of type T alongside the original command error.
//
// Use the typed error to preserve a command's exit code:
//
//	exitErr, err := cli.Run[*exitCodeError](os.Args, run)
//	if exitErr != nil {
//		os.Exit(exitErr.code)
//	}
//	if err != nil {
//		os.Exit(1)
//	}
func Run[T error](argv []string, operator func([]string) error) (T, error) {
	err := dispatch(argv[1:], operator)
	status, _ := errors.AsType[T](err)

	return status, err
}

func dispatch(args []string, operator func([]string) error) error {
	command := operator
	switch {
	case len(args) >= 2 && args[0] == "ci" && args[1] == "retirement":
		command = retirement.Command
		args = args[2:]
	case len(args) > 0 && args[0] == "lint-hcl-sources":
		command = hclsources.FileCommand
		args = args[1:]
	}

	return command(args)
}
