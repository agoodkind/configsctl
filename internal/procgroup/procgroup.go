// Package procgroup runs commands with context cancellation.
// On Unix without a controlling terminal, cancellation signals the child process group.
package procgroup

import "time"

// StopGrace gives a canceled Unix command time to exit after SIGINT before
// os/exec kills the child process.
const StopGrace = 20 * time.Second
