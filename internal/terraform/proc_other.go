//go:build !unix

package terraform

import "os/exec"

// detachFromTerminal is a no-op where process groups are not available.
func detachFromTerminal(*exec.Cmd) {}
