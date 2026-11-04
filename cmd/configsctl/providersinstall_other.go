//go:build !unix

package main

import "errors"

// processRunning returns an error on non-Unix systems, where this tool does
// not build. The error prevents deleting a staging directory of a live process.
func processRunning(_ int) (bool, error) {
	return false, errors.New("process inspection is not available on this platform")
}
