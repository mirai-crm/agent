//go:build !windows

package updater

import (
	"io"
	"os/exec"
	"strconv"
	"syscall"
)

func startDetachedCommand(helperPath, targetPath, configPath string, parentPID int) error {
	execCmd := exec.Command(
		helperPath,
		"apply-update",
		"--target", targetPath,
		"--config", configPath,
		"--parent-pid", strconv.Itoa(parentPID),
	)
	execCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	execCmd.Stdout = io.Discard
	execCmd.Stderr = io.Discard
	return execCmd.Start()
}
