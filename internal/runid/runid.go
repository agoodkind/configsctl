// Package runid creates the identifier of one deploy or OpenTofu run. The id
// sorts by start time and has a random suffix.
package runid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"

	"goodkind.io/configsctl/internal/clock"
)

// EnvVar defines the environment key that Current reads for the gate run ID.
const EnvVar = "CONFIGS_RUN_ID"

const suffixBytes = 4

// New returns an id in the form 20060102T150405Z-0a1b2c3d.
func New() (string, error) {
	suffix := make([]byte, suffixBytes)
	if _, err := rand.Read(suffix); err != nil {
		slog.Error("runid.random_failed", "err", err)
		return "", fmt.Errorf("read random run id suffix: %w", err)
	}
	return clock.FileStamp() + "-" + hex.EncodeToString(suffix), nil
}

// Current returns the run id from EnvVar, or a new id when the variable is
// empty.
func Current() (string, error) {
	if id := os.Getenv(EnvVar); id != "" {
		return id, nil
	}
	return New()
}
