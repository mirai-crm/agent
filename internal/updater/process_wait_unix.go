//go:build !windows

package updater

import (
	"context"
	"fmt"
	"syscall"
	"time"
)

func waitForParentExit(ctx context.Context, pid int) error {
	for {
		err := syscall.Kill(pid, 0)
		switch err {
		case nil, syscall.EPERM:
			if err := sleepWithContext(ctx, 200*time.Millisecond); err != nil {
				return err
			}
		case syscall.ESRCH:
			return nil
		default:
			return fmt.Errorf("probe parent process: %w", err)
		}
	}
}
