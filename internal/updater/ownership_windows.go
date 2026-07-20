//go:build windows

package updater

import "os"

func restoreFileOwnership(string, os.FileInfo) error {
	return nil
}
