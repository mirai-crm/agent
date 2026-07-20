package updater

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

type LaunchResult struct {
	HelperPath  string
	RequestPath string
}

type launchedCommand struct {
	Path        string
	Args        []string
	SysProcAttr *syscall.SysProcAttr
}

type launchDeps struct {
	selfPath      string
	startDetached func(launchedCommand) error
}

func LaunchHelper(req ApplyRequest) (LaunchResult, error) {
	selfPath, err := os.Executable()
	if err != nil {
		return LaunchResult{}, fmt.Errorf("resolve current executable: %w", err)
	}
	return launchHelperWith(req, launchDeps{
		selfPath:      selfPath,
		startDetached: startDetachedCommand,
	})
}

func launchHelperWith(req ApplyRequest, deps launchDeps) (LaunchResult, error) {
	if err := req.Validate(); err != nil {
		return LaunchResult{}, fmt.Errorf("validate apply request: %w", err)
	}
	if deps.selfPath == "" {
		return LaunchResult{}, fmt.Errorf("helper source executable is required")
	}
	if deps.startDetached == nil {
		return LaunchResult{}, fmt.Errorf("detached launcher is required")
	}

	helperDir := filepath.Dir(req.StagedBinaryPath)
	requestPath, err := WriteApplyRequest(helperDir, req)
	if err != nil {
		return LaunchResult{}, err
	}
	helperPath, err := copyHelperBinary(deps.selfPath, helperDir)
	if err != nil {
		return LaunchResult{}, err
	}
	if err := prepareHelperSidecarDLL(req, helperDir); err != nil {
		return LaunchResult{}, err
	}
	cmd := buildHelperCommand(helperPath, requestPath)
	if err := deps.startDetached(cmd); err != nil {
		return LaunchResult{}, fmt.Errorf("launch helper: %w", err)
	}
	return LaunchResult{HelperPath: helperPath, RequestPath: requestPath}, nil
}

func copyHelperBinary(srcPath, dir string) (string, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("open helper source: %w", err)
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return "", fmt.Errorf("stat helper source: %w", err)
	}

	pattern := ".mirai-agent-update-helper-*"
	if ext := filepath.Ext(srcPath); ext != "" {
		pattern += ext
	}
	dst, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("create helper copy: %w", err)
	}
	dstPath := dst.Name()
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return "", fmt.Errorf("copy helper binary: %w", err)
	}
	if err := dst.Close(); err != nil {
		return "", fmt.Errorf("close helper binary: %w", err)
	}
	if err := os.Chmod(dstPath, info.Mode().Perm()); err != nil && runtime.GOOS != "windows" {
		return "", fmt.Errorf("chmod helper binary: %w", err)
	}
	return dstPath, nil
}

func copyFileContents(srcPath, dstPath string) error {
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("stat copy source: %w", err)
	}
	if dstInfo, err := os.Stat(dstPath); err == nil && os.SameFile(srcInfo, dstInfo) {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat copy destination: %w", err)
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open copy source: %w", err)
	}
	defer src.Close()
	dst, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("create copy destination: %w", err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy file: %w", err)
	}
	return nil
}
