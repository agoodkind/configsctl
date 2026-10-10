//go:build !unix

package procgroup

import "os/exec"

// RunGroup requires a command created by [exec.CommandContext].
func RunGroup(cmd *exec.Cmd) error {
	return Run(cmd)
}
