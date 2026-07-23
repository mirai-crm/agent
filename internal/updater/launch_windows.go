//go:build windows

package updater

import (
	"io"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

func startDetachedCommand(helperPath, targetPath, configPath string, parentPID int) error {
	execCmd := exec.Command(
		helperPath,
		"apply-update",
		"--target", targetPath,
		"--config", configPath,
		"--parent-pid", strconv.Itoa(parentPID),
	)
	execCmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	execCmd.Stdout = io.Discard
	execCmd.Stderr = io.Discard
	return execCmd.Start()
}
