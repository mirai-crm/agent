//go:build !windows

package updater

import (
	"os"
	"path/filepath"
)

func atomicReplace(srcPath, dstPath string) error {
	return os.Rename(srcPath, dstPath)
}

func syncParentDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func removeArtifact(path string) error {
	return os.Remove(path)
}

func removeStageDir(path string) error {
	return os.Remove(path)
}
