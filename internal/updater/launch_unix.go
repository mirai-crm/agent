//go:build !windows

package updater

import (
	"io"
	"os/exec"
	"syscall"
)

func buildHelperCommand(helperPath, requestPath string) launchedCommand {
	return launchedCommand{
		Path:        helperPath,
		Args:        []string{"apply-update", requestPath},
		SysProcAttr: &syscall.SysProcAttr{Setsid: true},
	}
}

func startDetachedCommand(cmd launchedCommand) error {
	execCmd := exec.Command(cmd.Path, cmd.Args...)
	execCmd.SysProcAttr = cmd.SysProcAttr
	execCmd.Stdout = io.Discard
	execCmd.Stderr = io.Discard
	return execCmd.Start()
}

func prepareHelperSidecarDLL(ApplyRequest, string) error {
	return nil
}
