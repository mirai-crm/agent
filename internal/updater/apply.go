package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
}

type realServiceController struct{}

func (realServiceController) Start(configPath string) error { return svc.Start(configPath) }
func (realServiceController) Stop(configPath string) error  { return svc.Stop(configPath) }

type replacementState struct {
	binary backupEntry
	dll    backupEntry
}

type backupEntry struct {
	path             string
	backupPath       string
	originalExists   bool
	originalInfo     os.FileInfo
	backupCreated    bool
	currentInstalled bool
}

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
	if deps.waitForParent == nil {
		return fmt.Errorf("waitForParent dependency is required")
	}
	if deps.service == nil {
		return fmt.Errorf("service dependency is required")
	}
	if deps.sleep == nil {
		deps.sleep = sleepWithContext
	}
	if deps.pollInterval <= 0 {
		deps.pollInterval = defaultHealthPollInterval
	}

	waitCtx, cancelWait := context.WithTimeout(ctx, time.Duration(req.ParentExitTimeoutMillis)*time.Millisecond)
	err := deps.waitForParent(waitCtx, req.ParentPID)
	cancelWait()
	if err != nil {
		return fmt.Errorf("wait for parent exit: %w", err)
	}
	if err := writePendingHealthMarker(req); err != nil {
		return fmt.Errorf("write pending health marker: %w", err)
	}

	var state replacementState
	entry, err := replaceOne(req.TargetPath, req.StagedBinaryPath, req.Nonce, false)
	state.binary = entry
	if err != nil {
		return rollbackAfterFailure(req, deps, state, fmt.Errorf("replace files: %w", err))
	}
	if req.StagedLibUSBPath != "" {
		entry, err = replaceOne(targetDLLPath(req.TargetPath), req.StagedLibUSBPath, req.Nonce, true)
		state.dll = entry
		if err != nil {
			return rollbackAfterFailure(req, deps, state, fmt.Errorf("replace files: %w", err))
		}
	}

	if err := deps.service.Start(req.ConfigPath); err != nil {
		return rollbackAfterFailure(req, deps, state, fmt.Errorf("start updated service: %w", err))
	}
	if err := waitForHealthyMarker(ctx, req, deps); err != nil {
		return rollbackAfterFailure(req, deps, state, fmt.Errorf("wait for healthy service: %w", err))
	}
	if err := cleanupSuccessfulApply(req, state); err != nil {
		return fmt.Errorf("cleanup healthy update: %w", err)
	}
	return nil
}

func rollbackAfterFailure(req ApplyRequest, deps applyDeps, state replacementState, cause error) error {
	var errs []error
	if err := deps.service.Stop(req.ConfigPath); err != nil {
		errs = append(errs, fmt.Errorf("stop updated service: %w", err))
	}
	if err := rollbackReplacement(state); err != nil {
		errs = append(errs, fmt.Errorf("restore previous files: %w", err))
	}
	if err := deps.service.Start(req.ConfigPath); err != nil {
		errs = append(errs, fmt.Errorf("restart previous service: %w", err))
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
		if err == nil && ok && marker.State == healthStateHealthy && marker.Nonce == req.Nonce && marker.Version == req.Version {
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

func cleanupSuccessfulApply(req ApplyRequest, state replacementState) error {
	for _, path := range []string{
		backupPathIfCreated(state.dll),
		backupPathIfCreated(state.binary),
		markerPath(req.ConfigPath),
	} {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func backupPathIfCreated(entry backupEntry) string {
	if !entry.backupCreated {
		return ""
	}
	return entry.backupPath
}

func rollbackReplacement(state replacementState) error {
	var errs []error
	for _, entry := range []backupEntry{state.dll, state.binary} {
		if entry.path == "" {
			continue
		}
		if err := restoreEntry(entry); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func restoreEntry(entry backupEntry) error {
	if entry.originalExists && !entry.backupCreated {
		return nil
	}
	if !entry.originalExists {
		if !entry.currentInstalled {
			return nil
		}
		if err := os.Remove(entry.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove introduced file %s: %w", entry.path, err)
		}
		return nil
	}
	if entry.currentInstalled {
		if err := os.Remove(entry.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove replacement %s: %w", entry.path, err)
		}
	}
	if err := os.Rename(entry.backupPath, entry.path); err != nil {
		return fmt.Errorf("restore backup %s: %w", entry.path, err)
	}
	return nil
}

func replaceOne(targetPath, stagedPath, nonce string, allowMissingOriginal bool) (backupEntry, error) {
	entry := backupEntry{
		path:       targetPath,
		backupPath: backupPathFor(targetPath, nonce),
	}

	stagedInfo, err := os.Stat(stagedPath)
	if err != nil {
		return entry, fmt.Errorf("stat staged file %s: %w", stagedPath, err)
	}
	if !stagedInfo.Mode().IsRegular() {
		return entry, fmt.Errorf("staged file %s must be regular", stagedPath)
	}

	info, err := os.Stat(targetPath)
	switch {
	case err == nil:
		entry.originalExists = true
		entry.originalInfo = info
	case os.IsNotExist(err) && allowMissingOriginal:
	default:
		return entry, fmt.Errorf("stat target file %s: %w", targetPath, err)
	}

	if entry.originalExists {
		if _, err := os.Stat(entry.backupPath); err == nil {
			return entry, fmt.Errorf("backup path %s already exists", entry.backupPath)
		} else if err != nil && !os.IsNotExist(err) {
			return entry, fmt.Errorf("stat backup file %s: %w", entry.backupPath, err)
		}
		if err := os.Rename(targetPath, entry.backupPath); err != nil {
			return entry, fmt.Errorf("backup target file %s: %w", targetPath, err)
		}
		entry.backupCreated = true
	}

	if err := os.Rename(stagedPath, targetPath); err != nil {
		return entry, fmt.Errorf("install staged file %s: %w", targetPath, err)
	}
	entry.currentInstalled = true
	if entry.originalExists {
		if err := restoreFileMetadata(targetPath, entry.originalInfo); err != nil {
			return entry, fmt.Errorf("preserve target metadata %s: %w", targetPath, err)
		}
	}
	return entry, nil
}

func backupPathFor(path, nonce string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".backup-"+nonce)
}

func targetDLLPath(targetPath string) string {
	return filepath.Join(filepath.Dir(targetPath), "libusb-1.0.dll")
}

func restoreFileMetadata(path string, info os.FileInfo) error {
	if info == nil {
		return nil
	}
	if err := os.Chmod(path, info.Mode().Perm()); err != nil && !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("chmod replacement %s: %w", path, err)
	}
	return restoreFileOwnership(path, info)
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
