//go:build !unix

package installedsource

import (
	"errors"
	"io/fs"
)

func checkOwnerIDs(_ string, _ fs.FileInfo, _, _ int64) error {
	return errors.New("installed source verification requires Unix file ownership")
}
