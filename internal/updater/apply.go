package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mirai-agent/mirai-agent/internal/svc"
)

const defaultHealthPollInterval = 200 * time.Millisecond

type serviceController interface {
	Start(configPath string) error
	Stop(configPath string) error
}

type applyDeps struct {
	waitForParent func(context.Context, int) error
	service       serviceController
	sleep         func(context.Context, time.Duration) error
	pollInterval  time.Duration
	afterPhase    func(updatePhase) error
}

type realServiceController struct{}

func (realServiceController) Start(configPath string) error { return svc.Start(configPath) }
func (realServiceController) Stop(configPath string) error  { return svc.Stop(configPath) }

func Apply(ctx context.Context, req ApplyRequest) error {
	return applyWithDeps(ctx, req, applyDeps{
		waitForParent: waitForParentExit,
		service:       realServiceController{},
		sleep:         sleepWithContext,
		pollInterval:  defaultHealthPollInterval,
	})
}

func applyWithDeps(ctx context.Context, req ApplyRequest, deps applyDeps) error {
	if err := req.Validate(); err != nil {
		return fmt.Errorf("validate apply request: %w", err)
	}
	if deps.waitForParent == nil || deps.service == nil {
		return fmt.Errorf("apply dependencies are required")
	}
	if deps.sleep == nil {
		deps.sleep = sleepWithContext
	}
	if deps.pollInterval <= 0 {
		deps.pollInterval = defaultHealthPollInterval
	}

	existingMarker, markerExists, err := tryLoadHealthMarker(markerPath(req.ConfigPath))
	if err != nil {
		return fmt.Errorf("inspect update journal: %w", err)
	}
	if markerExists && existingMarker.State == healthStateHealthy && existingMarker.Phase == phaseHealthy &&
		existingMarker.Nonce == req.Nonce && existingMarker.Version == req.Version {
		return cleanupHealthyApply(req, existingMarker)
	}
	cleanupAbortedApply := cleanupGeneratedArtifacts
	if markerExists {
		cleanupAbortedApply = cleanupTransientArtifacts
	}

	if err := deps.service.Stop(req.ConfigPath); err != nil {
		cause := fmt.Errorf("stop service before update: %w", err)
		if cleanupErr := cleanupAbortedApply(req); cleanupErr != nil {
			return errors.Join(cause, cleanupErr)
		}
		return cause
	}

	waitCtx, cancelWait := context.WithTimeout(ctx, time.Duration(req.ParentExitTimeoutMillis)*time.Millisecond)
	err = deps.waitForParent(waitCtx, req.ParentPID)
	cancelWait()
	if err != nil {
		errs := []error{fmt.Errorf("wait for parent exit: %w", err)}
		if cleanupErr := cleanupAbortedApply(req); cleanupErr != nil {
			errs = append(errs, cleanupErr)
		}
		return errors.Join(errs...)
	}

	marker, err := loadOrCreateApplyMarker(req, deps)
	if err != nil {
		return err
	}
	if marker.State == healthStateHealthy {
		return cleanupHealthyApply(req, marker)
	}

	if phaseBefore(marker.Phase, phaseBackupsReady) {
		if err := prepareBackups(&marker); err != nil {
			return err
		}
		if err := persistPhase(req, &marker, phaseBackupsReady, deps); err != nil {
			return err
		}
	}
	if phaseBefore(marker.Phase, phaseBinaryReplaced) {
		if err := persistPhase(req, &marker, phaseBinaryReplacing, deps); err != nil {
			return err
		}
		// Binary-first invariant: targetPath is atomically replaced and never
		// absent. A crash may leave the new binary beside the old DLL, but the
		// journal resumes DLL replacement before health can be confirmed.
		if err := installAtomically(marker.StagedBinaryPath, marker.TargetPath); err != nil {
			return rollbackAfterFailure(req, deps, marker, fmt.Errorf("replace binary: %w", err))
		}
		if err := persistPhase(req, &marker, phaseBinaryReplaced, deps); err != nil {
			return err
		}
	}
	if phaseBefore(marker.Phase, phaseDLLReplaced) {
		if marker.StagedLibUSBPath != "" {
			if err := persistPhase(req, &marker, phaseDLLReplacing, deps); err != nil {
				return err
			}
			if err := installAtomically(marker.StagedLibUSBPath, targetDLLPath(marker.TargetPath)); err != nil {
				return rollbackAfterFailure(req, deps, marker, fmt.Errorf("replace DLL: %w", err))
			}
		}
		if err := persistPhase(req, &marker, phaseDLLReplaced, deps); err != nil {
			return err
		}
	}
	if phaseBefore(marker.Phase, phaseServiceStarted) {
		if err := persistPhase(req, &marker, phaseServiceStarted, deps); err != nil {
			return err
		}
		if err := deps.service.Start(req.ConfigPath); err != nil {
			return rollbackAfterFailure(req, deps, marker, fmt.Errorf("start updated service: %w", err))
		}
	}
	if err := waitForHealthyMarker(ctx, req, deps); err != nil {
		return rollbackAfterFailure(req, deps, marker, fmt.Errorf("wait for healthy service: %w", err))
	}
	healthy, err := loadHealthMarker(markerPath(req.ConfigPath))
	if err != nil {
		return fmt.Errorf("reload healthy marker: %w", err)
	}
	return cleanupHealthyApply(req, healthy)
}

func loadOrCreateApplyMarker(req ApplyRequest, deps applyDeps) (healthMarker, error) {
	marker, ok, err := tryLoadHealthMarker(markerPath(req.ConfigPath))
	if err != nil {
		return healthMarker{}, fmt.Errorf("load update journal: %w", err)
	}
	if !ok {
		if err := writePendingHealthMarker(req); err != nil {
			return healthMarker{}, fmt.Errorf("write pending health marker: %w", err)
		}
		marker, err = loadHealthMarker(markerPath(req.ConfigPath))
		if err != nil {
			return healthMarker{}, err
		}
		if err := callAfterPhase(deps, phasePrepared); err != nil {
			return healthMarker{}, err
		}
		return marker, nil
	}
	if marker.Nonce != req.Nonce || marker.Version != req.Version ||
		marker.TargetPath != req.TargetPath ||
		marker.StagedBinaryPath != req.StagedBinaryPath ||
		marker.StagedLibUSBPath != req.StagedLibUSBPath {
		return healthMarker{}, fmt.Errorf("update journal does not match request")
	}
	return marker, nil
}

func prepareBackups(marker *healthMarker) error {
	targetInfo, err := os.Stat(marker.TargetPath)
	if err != nil {
		return fmt.Errorf("stat target binary: %w", err)
	}
	if err := copySynced(marker.TargetPath, marker.BinaryBackupPath, targetInfo); err != nil {
		return fmt.Errorf("backup target binary: %w", err)
	}

	dllPath := targetDLLPath(marker.TargetPath)
	dllInfo, err := os.Stat(dllPath)
	switch {
	case err == nil:
		marker.DLLHadOriginal = true
		if err := copySynced(dllPath, marker.DLLBackupPath, dllInfo); err != nil {
			return fmt.Errorf("backup target DLL: %w", err)
		}
	case os.IsNotExist(err):
		marker.DLLHadOriginal = false
		_ = os.Remove(marker.DLLBackupPath)
	default:
		return fmt.Errorf("stat target DLL: %w", err)
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
	tmpPath := tmp
	defer os.Remove(tmpPath)
	if err := atomicReplace(tmpPath, targetPath); err != nil {
		return err
	}
	return syncParentDir(targetPath)
}

func copySynced(srcPath, dstPath string, info os.FileInfo) error {
	tmp, err := copyToTemp(srcPath, filepath.Dir(dstPath), info)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := atomicReplace(tmp, dstPath); err != nil {
		return err
	}
	return syncParentDir(dstPath)
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

func persistPhase(req ApplyRequest, marker *healthMarker, phase updatePhase, deps applyDeps) error {
	marker.Phase = phase
	if err := writeHealthMarker(markerPath(req.ConfigPath), *marker); err != nil {
		return fmt.Errorf("persist update phase %s: %w", phase, err)
	}
	return callAfterPhase(deps, phase)
}

func callAfterPhase(deps applyDeps, phase updatePhase) error {
	if deps.afterPhase == nil {
		return nil
	}
	return deps.afterPhase(phase)
}

func rollbackAfterFailure(req ApplyRequest, deps applyDeps, marker healthMarker, cause error) error {
	var errs []error
	if err := deps.service.Stop(req.ConfigPath); err != nil {
		errs = append(errs, fmt.Errorf("stop updated service: %w", err))
	}
	if marker.DLLHadOriginal {
		if err := installAtomically(marker.DLLBackupPath, targetDLLPath(marker.TargetPath)); err != nil {
			errs = append(errs, fmt.Errorf("restore previous DLL: %w", err))
		}
	} else if err := os.Remove(targetDLLPath(marker.TargetPath)); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove introduced DLL: %w", err))
	}
	if err := installAtomically(marker.BinaryBackupPath, marker.TargetPath); err != nil {
		errs = append(errs, fmt.Errorf("restore previous binary: %w", err))
	}
	if len(errs) == 0 {
		if err := deps.service.Start(req.ConfigPath); err != nil {
			errs = append(errs, fmt.Errorf("restart previous service: %w", err))
		}
	}
	if len(errs) == 0 {
		if err := cleanupHandledApply(req, marker); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return cause
	}
	return errors.Join(append([]error{cause}, errs...)...)
}

func waitForHealthyMarker(ctx context.Context, req ApplyRequest, deps applyDeps) error {
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(req.HealthTimeoutMillis)*time.Millisecond)
	defer cancel()
	for {
		marker, ok, err := tryLoadHealthMarker(markerPath(req.ConfigPath))
		if err == nil && ok && marker.State == healthStateHealthy && marker.Phase == phaseHealthy &&
			marker.Nonce == req.Nonce && marker.Version == req.Version {
			return nil
		}
		if waitCtx.Err() != nil {
			return waitCtx.Err()
		}
		if err := deps.sleep(waitCtx, deps.pollInterval); err != nil {
			return err
		}
	}
}

func cleanupHealthyApply(req ApplyRequest, marker healthMarker) error {
	if err := removeFiles(marker.BinaryBackupPath, marker.DLLBackupPath, markerPath(req.ConfigPath)); err != nil {
		return err
	}
	return cleanupGeneratedArtifacts(req)
}

func cleanupHandledApply(req ApplyRequest, marker healthMarker) error {
	if err := removeFiles(marker.BinaryBackupPath, marker.DLLBackupPath, markerPath(req.ConfigPath)); err != nil {
		return err
	}
	return cleanupGeneratedArtifacts(req)
}

func removeFiles(paths ...string) error {
	var errs []error
	for _, path := range paths {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func backupPathFor(path, nonce string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".backup-"+nonce)
}

func targetDLLPath(targetPath string) string {
	return filepath.Join(filepath.Dir(targetPath), "libusb-1.0.dll")
}

func phaseBefore(got, want updatePhase) bool {
	order := map[updatePhase]int{
		phasePrepared: 0, phaseBackupsReady: 1,
		phaseBinaryReplacing: 2, phaseBinaryReplaced: 3,
		phaseDLLReplacing: 4, phaseDLLReplaced: 5,
		phaseServiceStarted: 6, phaseHealthy: 7,
	}
	return order[got] < order[want]
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
