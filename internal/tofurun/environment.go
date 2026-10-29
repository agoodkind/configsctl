package tofurun

import (
	"errors"
)

// childEnvironment returns Request.Environment unchanged and rejects nil.
func childEnvironment(request Request) ([]string, error) {
	if request.Environment == nil {
		return nil, errors.New("tofurun: set Request.Environment to the complete child environment")
	}
	return request.Environment, nil
}
