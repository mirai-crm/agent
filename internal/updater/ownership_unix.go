//go:build !windows

package updater

import (
	"os"
	"syscall"
)

func restoreFileOwnership(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return os.Chown(path, int(stat.Uid), int(stat.Gid))
}
