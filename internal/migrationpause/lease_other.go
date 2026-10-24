//go:build !unix

package migrationpause

import "errors"

// AcquireWriter refuses platforms without the required native file locks.
func AcquireWriter(_ string) (func(), error) {
	return nil, errors.New("infrastructure migration leases require Unix file locks")
}

// AcquireExclusive refuses platforms without the required native file locks.
func AcquireExclusive(_ string) (func(), error) {
	return nil, errors.New("infrastructure migration leases require Unix file locks")
}
