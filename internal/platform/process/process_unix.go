//go:build aix || android || darwin || dragonfly || freebsd || hurd || illumos || ios || linux || netbsd || openbsd || solaris

package process

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// signalNames covers the signals a shell command commonly dies from.
var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT",
	syscall.SIGILL: "SIGILL", syscall.SIGTRAP: "SIGTRAP", syscall.SIGABRT: "SIGABRT",
	syscall.SIGBUS: "SIGBUS", syscall.SIGFPE: "SIGFPE", syscall.SIGKILL: "SIGKILL",
	syscall.SIGUSR1: "SIGUSR1", syscall.SIGSEGV: "SIGSEGV", syscall.SIGUSR2: "SIGUSR2",
	syscall.SIGPIPE: "SIGPIPE", syscall.SIGALRM: "SIGALRM", syscall.SIGTERM: "SIGTERM",
}

func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(command *exec.Cmd) {
	if command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
}

func terminateProcessGroup(command *exec.Cmd) {
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
}

func canEnter(path string) bool {
	return syscall.Access(path, 1) == nil // X_OK: entering the caller's cwd requires search permission
}

// exitSignal names the signal that terminated the process, if any.
func exitSignal(state *os.ProcessState) string {
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return ""
	}
	if name, known := signalNames[status.Signal()]; known {
		return name
	}
	return fmt.Sprintf("signal %d", int(status.Signal()))
}
