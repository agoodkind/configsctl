package gate

import (
	"fmt"
	"regexp"
)

// printableArg accepts printable ASCII only. The gate passes every argument to
// a child process as one argv entry and never through a shell.
var printableArg = regexp.MustCompile(`^[\x20-\x7E]+$`)

func checkedArgs(args []string) ([]string, error) {
	safe := make([]string, 0, len(args))
	for _, arg := range args {
		match := printableArg.FindString(arg)
		if match != arg {
			return nil, fmt.Errorf("argument %q contains a non-printable character", arg)
		}
		safe = append(safe, match)
	}
	return safe, nil
}
