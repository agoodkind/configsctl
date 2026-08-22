//go:build !unix

package main

import (
	"errors"
	"io/fs"
)

// ownedByCurrentUser fails closed off unix, where this tool is not built and
// where file ownership has no portable answer. Returning an error refuses the
// run log directory rather than trusting an entry nobody checked.
func ownedByCurrentUser(_ fs.FileInfo) (bool, error) {
	return false, errors.New("file ownership is not available on this platform")
}
