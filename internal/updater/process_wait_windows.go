//go:build windows

package updater

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

func waitForParentExit(ctx context.Context, pid int) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return err
	}
	defer windows.CloseHandle(handle)

	for {
		status, err := windows.WaitForSingleObject(handle, 200)
		if err != nil {
			return err
		}
		switch uint32(status) {
		case uint32(windows.WAIT_OBJECT_0):
			return nil
		case uint32(windows.WAIT_TIMEOUT):
			if err := sleepWithContext(ctx, 10*time.Millisecond); err != nil {
				return err
			}
		default:
			return fmt.Errorf("wait status %d", status)
		}
	}
}
