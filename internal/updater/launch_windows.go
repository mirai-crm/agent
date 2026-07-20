//go:build windows

package updater

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

func buildHelperCommand(helperPath, requestPath string) launchedCommand {
	return launchedCommand{
		Path: helperPath,
		Args: []string{"apply-update", requestPath},
		SysProcAttr: &syscall.SysProcAttr{
			CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		},
	}
}

func startDetachedCommand(cmd launchedCommand) error {
	execCmd := exec.Command(cmd.Path, cmd.Args...)
	execCmd.SysProcAttr = cmd.SysProcAttr
	execCmd.Stdout = io.Discard
	execCmd.Stderr = io.Discard
	return execCmd.Start()
}

func prepareHelperSidecarDLL(req ApplyRequest, helperDir string) error {
	srcPath := req.StagedLibUSBPath
	if srcPath == "" {
		existing := targetDLLPath(req.TargetPath)
		if _, err := os.Stat(existing); err == nil {
			srcPath = existing
		}
	}
	if srcPath == "" {
		return nil
	}
	return copyFileContents(srcPath, filepath.Join(helperDir, "libusb-1.0.dll"))
}

func copyFileContents(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open dll source: %w", err)
	}
	defer src.Close()
	dst, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("create dll copy: %w", err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy dll: %w", err)
	}
	return nil
}
