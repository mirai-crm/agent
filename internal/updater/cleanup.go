package updater

import (
	"errors"
	"os"
)

func cleanupGeneratedArtifacts(req ApplyRequest) error {
	errs := []error{
		cleanupTransientArtifacts(req),
		removeArtifacts(req.StagedBinaryPath, req.StagedLibUSBPath),
	}
	if req.StageDir != "" {
		if err := removeStageDir(req.StageDir); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func cleanupTransientArtifacts(req ApplyRequest) error {
	return removeArtifacts(req.RequestPath, req.HelperSidecarPath, req.HelperPath)
}

func removeArtifacts(paths ...string) error {
	seen := make(map[string]bool)
	var errs []error
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if err := removeArtifact(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
