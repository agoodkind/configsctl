//go:build !unix

package statemigration

import (
	"errors"
	"os"
)

func requirePrivateOwner(_ os.FileInfo) error {
	return errors.New("state migration requires Unix private file ownership checks")
}

func lockExecutor(_ *os.File) error {
	return errors.New("state migration requires Unix executor locks")
}
