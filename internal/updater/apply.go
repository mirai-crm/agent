package updater

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/mirai-agent/mirai-agent/internal/svc"
)

const defaultParentExitTimeout = 30 * time.Second

func Apply(ctx context.Context, targetPath, configPath string, parentPID int) error {
	stagedPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve staged executable: %w", err)
	}
	if !filepath.IsAbs(targetPath) {
		return fmt.Errorf("target path must be absolute")
	}
	if !filepath.IsAbs(configPath) {
		return fmt.Errorf("config path must be absolute")
	}
	if parentPID <= 0 {
		return fmt.Errorf("parent PID must be positive")
	}
	if stagedPath == targetPath {
		return fmt.Errorf("staged and target paths must differ")
	}

	stageDir := filepath.Dir(stagedPath)
	defer cleanupStagedHelper(stageDir, stagedPath)

	if err := svc.Stop(configPath); err != nil {
		return fmt.Errorf("stop service before update: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, defaultParentExitTimeout)
	err = waitForParentExit(waitCtx, parentPID)
	cancel()
	if err != nil {
		return fmt.Errorf("wait for parent exit: %w", err)
	}

	if err := installAtomically(stagedPath, targetPath); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}
	if runtime.GOOS == "windows" {
		stagedDLL := filepath.Join(stageDir, "libusb-1.0.dll")
		if err := installAtomically(stagedDLL, targetDLLPath(targetPath)); err != nil {
			return fmt.Errorf("replace DLL: %w", err)
		}
	}
	if err := svc.Start(configPath); err != nil {
		return fmt.Errorf("start updated service: %w", err)
	}
	return nil
}

func installAtomically(stagedPath, targetPath string) error {
	targetInfo, err := os.Stat(targetPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if targetInfo == nil {
		targetInfo, err = os.Stat(stagedPath)
		if err != nil {
			return err
		}
	}
	tmp, err := copyToTemp(stagedPath, filepath.Dir(targetPath), targetInfo)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := atomicReplace(tmp, targetPath); err != nil {
		return err
	}
	return syncParentDir(targetPath)
}

func copyToTemp(srcPath, dir string, info os.FileInfo) (string, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(dir, ".mirai-agent-replace-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, src); err != nil {
		return "", err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		return "", err
	}
	if err := restoreFileOwnership(tmpPath, info); err != nil && !os.IsPermission(err) {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	ok = true
	return tmpPath, nil
}

func cleanupStagedHelper(stageDir, stagedPath string) {
	_ = removeArtifact(stagedPath)
	if runtime.GOOS == "windows" {
		_ = removeArtifact(filepath.Join(stageDir, "libusb-1.0.dll"))
	}
	_ = removeStageDir(stageDir)
}

func targetDLLPath(targetPath string) string {
	return filepath.Join(filepath.Dir(targetPath), "libusb-1.0.dll")
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
