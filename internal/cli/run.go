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
//
// Example:
//
//	status, err := cli.Run[*statusError](os.Args, operator)
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
