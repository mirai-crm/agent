package updater

import (
	"errors"
	"os"
)

func cleanupGeneratedArtifacts(req ApplyRequest) error {
	seen := make(map[string]bool)
	var errs []error
	for _, path := range []string{
		req.RequestPath,
		req.StagedBinaryPath,
		req.StagedLibUSBPath,
		req.HelperSidecarPath,
		req.HelperPath,
	} {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if err := removeArtifact(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	if req.StageDir != "" {
		if err := removeStageDir(req.StageDir); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
