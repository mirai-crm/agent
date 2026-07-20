//go:build windows

package updater

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func atomicReplace(srcPath, dstPath string) error {
	src, err := windows.UTF16PtrFromString(srcPath)
	if err != nil {
		return err
	}
	dst, err := windows.UTF16PtrFromString(dstPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(src, dst, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func syncParentDir(string) error {
	return nil
}

func removeArtifact(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return err
	}
	ptr, ptrErr := windows.UTF16PtrFromString(path)
	if ptrErr != nil {
		return errors.Join(err, ptrErr)
	}
	if delayErr := windows.MoveFileEx(ptr, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT); delayErr != nil {
		return errors.Join(err, delayErr)
	}
	return nil
}

func removeStageDir(path string) error {
	return removeArtifact(path)
}
