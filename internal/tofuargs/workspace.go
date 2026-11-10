package tofuargs

import "strings"

// WorkspaceDestination returns the workspace that a named state command uses.
// Other commands retain the current workspace.
func WorkspaceDestination(args []string, current string) string {
	command := Subcommand(args)
	if command != "workspace" && command != "env" {
		return current
	}

	for index, argument := range args {
		if argument != string(command) {
			continue
		}

		if index+1 >= len(args) {
			return current
		}

		action := args[index+1]
		if action != "new" && action != "select" && action != "delete" {
			return current
		}

		return workspaceName(args[index+2:], current)
	}

	return current
}

func workspaceName(args []string, current string) string {
	options := true

	for index := 0; index < len(args); index++ {
		argument := args[index]
		if options && argument == "--" {
			options = false
			continue
		}

		if options && strings.HasPrefix(argument, "-") {
			name, _, _ := strings.Cut(strings.TrimLeft(argument, "-"), "=")
			if name != "or-create" && name != "force" && ConsumesValue(argument) {
				index++
			}

			continue
		}

		return argument
	}

	return current
}
