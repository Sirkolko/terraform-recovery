//go:build unix

package terraform

import (
	"os/exec"
	"syscall"
)

// detachFromTerminal starts Terraform in its own process group, so that
// Ctrl+C in the terminal reaches only terraform-recovery. The tool then
// decides what to do: it interrupts a plan, but lets an import apply finish.
func detachFromTerminal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
