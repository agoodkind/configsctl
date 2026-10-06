//go:build !unix

package procgroup

import "os/exec"

// Run requires a command created by [exec.CommandContext].
// On non-Unix systems, cancellation kills only the child process.
func Run(cmd *exec.Cmd) error {
	cmd.WaitDelay = StopGrace
	return cmd.Run()
}
