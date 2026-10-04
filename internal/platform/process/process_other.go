//go:build !(aix || android || darwin || dragonfly || freebsd || hurd || illumos || ios || linux || netbsd || openbsd || solaris)

package process

import (
	"os"
	"os/exec"
)

func configureProcess(*exec.Cmd) {}

func killProcessGroup(command *exec.Cmd) {
	if command.Process != nil {
		_ = command.Process.Kill()
	}
}

// exitSignal reports no signal on platforms without POSIX wait status.
func exitSignal(*os.ProcessState) string { return "" }
