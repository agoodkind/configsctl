//go:build !unix

// Package fileowner reports whether the current user owns a file system entry.
package fileowner

import (
	"errors"
	"io/fs"
)

// CurrentUser returns an error on non-Unix systems, where this tool does not
// build and portable file ownership information is unavailable. The error
// prevents use of an entry without an ownership check.
func CurrentUser(_ fs.FileInfo) (bool, error) {
	return false, errors.New("file ownership is not available on this platform")
}
