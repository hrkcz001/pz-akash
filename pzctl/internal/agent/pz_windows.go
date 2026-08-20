//go:build windows

package agent

import "os/exec"

// The agent runs on Linux in production; these exist so the package builds and
// its tests run on the workstation where config.yaml is edited (the step 4 gate).
//
// Windows has no process groups in the POSIX sense, so there is no polite signal
// to send: a graceful stop has to come from the quit command written to stdin,
// and the escalation path goes straight to a kill.
func configureProcAttr(*exec.Cmd) {}

func terminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
