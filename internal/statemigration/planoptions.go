package statemigration

import "errors"

// PlanOptions controls native plan execution without changing migration ownership.
type PlanOptions struct {
	// Parallelism uses the native default when zero and an explicit limit when positive.
	Parallelism int
}

func (options PlanOptions) validate() error {
	if options.Parallelism < 0 {
		return errors.New("migration plan parallelism must be positive when specified")
	}
	return nil
}
