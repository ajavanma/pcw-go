//go:build !linux && !darwin

package pcw

import "os/exec"

// Other systems retain CommandContext's direct-process cancellation. Process
// tree cleanup is supported only on Linux and macOS.
func configureProcess(cmd *exec.Cmd) {}
func cleanupProcess(cmd *exec.Cmd)   {}
