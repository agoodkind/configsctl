// Package procgroup runs commands with context cancellation.
// On Unix without a controlling terminal, Run cancellation signals the child process group.
// On Unix, RunGroup cancellation sends SIGTERM to the child process group.
package procgroup

import "time"

// StopGrace gives Run's canceled Unix command time to exit after SIGINT before
// os/exec kills the child process.
// RunGroup uses StopGrace after SIGTERM and while waiting for output copies.
const StopGrace = 20 * time.Second
