//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// configureProcAttr puts PZ in its own process group.
//
// This is what makes a halt reliable. The launcher is a shell script that runs
// java as a child, so a signal sent to the script's pid is not forwarded to the
// JVM — v1 did exactly that, and its "graceful" shutdown depended on the
// container being destroyed rather than on the server saving and exiting.
// Signalling the negative pid reaches every process in the group.
func configureProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
