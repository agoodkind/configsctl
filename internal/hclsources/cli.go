package hclsources

import (
	"errors"
	"os"
)

var errFileUsage = errors.New("usage: configsctl lint-hcl-sources <request.json>")

// FileCommand checks source files from one JSON request.
//
// The caller supplies the request path as the only argument.
//
// Run the check from the command line with an existing request file:
//
//	configsctl lint-hcl-sources request.json
func FileCommand(args []string) error {
	if len(args) != 1 {
		return errFileUsage
	}

	data, err := os.ReadFile(args[0])
	if err != nil {
		return &os.PathError{Op: "read HCL source request", Path: args[0], Err: err}
	}

	return CheckRequest(data)
}
