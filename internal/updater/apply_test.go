package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestApplySwapsBinaryAndWaitsForHealthyMarker(t *testing.T) {
	fixture := newApplyFixture(t, applyFixtureOptions{withExistingDLL: true})
	configBefore := mustReadBytes(t, fixture.configPath)
	paymentsBefore := mustReadBytes(t, fixture.paymentsPath)

	service := &stubServiceController{
		startHook: func(configPath string) error {
			return MarkHealthy(configPath, fixture.request.Version)
		},
	}

	err := applyWithDeps(context.Background(), fixture.request, applyDeps{
		waitForParent: func(context.Context, int) error { return nil },
		service:       service,
		sleep:         testSleep,
		pollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("applyWithDeps() error = %v", err)
	}

	if got := mustReadString(t, fixture.targetPath); got != "new-binary" {
		t.Fatalf("target contents = %q, want new binary", got)
	}
	if got := mustReadString(t, fixture.targetDLLPath); got != "new-dll" {
		t.Fatalf("target dll contents = %q, want new dll", got)
	}
	if runtime.GOOS != "windows" {
		if got := filePerm(t, fixture.targetPath); got != 0o751 {
			t.Fatalf("target mode = %v, want 0751", got)
		}
	}
	if service.startCalls != 1 {
		t.Fatalf("start calls = %d, want 1", service.startCalls)
	}
	if service.stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", service.stopCalls)
	}
	if _, err := os.Stat(markerPath(fixture.configPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker stat error = %v, want not exists", err)
	}
	if _, err := os.Stat(backupPathFor(fixture.targetPath, fixture.request.Nonce)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("binary backup stat error = %v, want not exists", err)
	}
	if _, err := os.Stat(backupPathFor(fixture.targetDLLPath, fixture.request.Nonce)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dll backup stat error = %v, want not exists", err)
	}
	if got := mustReadBytes(t, fixture.configPath); string(got) != string(configBefore) {
		t.Fatalf("config changed:\n got: %q\nwant: %q", got, configBefore)
	}
	if got := mustReadBytes(t, fixture.paymentsPath); string(got) != string(paymentsBefore) {
		t.Fatalf("payments changed:\n got: %q\nwant: %q", got, paymentsBefore)
	}
}

func TestApplyHealthyResumeCleansWithoutServiceLifecycleCalls(t *testing.T) {
	fixture := newApplyFixture(t, applyFixtureOptions{withExistingDLL: true})
	stageDir := filepath.Dir(fixture.stagedBinary)
	fixture.request.StageDir = stageDir
	fixture.request.RequestPath = filepath.Join(stageDir, "request.json")
	fixture.request.HelperPath = filepath.Join(stageDir, "helper")
	writeFileWithMode(t, fixture.request.RequestPath, []byte("{}"), 0o600)
	writeFileWithMode(t, fixture.request.HelperPath, []byte("helper"), 0o700)

	marker := healthMarker{
		Nonce:            fixture.request.Nonce,
		Version:          fixture.request.Version,
		State:            healthStateHealthy,
		Phase:            phaseHealthy,
		TargetPath:       fixture.targetPath,
		StagedBinaryPath: fixture.stagedBinary,
		StagedLibUSBPath: fixture.stagedDLL,
		BinaryBackupPath: backupPathFor(fixture.targetPath, fixture.request.Nonce),
		DLLBackupPath:    backupPathFor(fixture.targetDLLPath, fixture.request.Nonce),
		DLLHadOriginal:   true,
	}
	writeFileWithMode(t, marker.BinaryBackupPath, []byte("old-binary"), 0o751)
	writeFileWithMode(t, marker.DLLBackupPath, []byte("old-dll"), 0o644)
	if err := writeHealthMarker(markerPath(fixture.configPath), marker); err != nil {
		t.Fatalf("writeHealthMarker() error = %v", err)
	}
	targetBefore := mustReadBytes(t, fixture.targetPath)
	dllBefore := mustReadBytes(t, fixture.targetDLLPath)
	waitCalled := false
	service := &stubServiceController{}

	err := applyWithDeps(context.Background(), fixture.request, applyDeps{
		waitForParent: func(context.Context, int) error {
			waitCalled = true
			return nil
		},
		service:      service,
		sleep:        testSleep,
		pollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("applyWithDeps() error = %v", err)
	}
	if waitCalled {
		t.Fatal("parent wait called for already healthy update")
	}
	if service.stopCalls != 0 || service.startCalls != 0 {
		t.Fatalf("service calls = stop %d, start %d; want none", service.stopCalls, service.startCalls)
	}
	if got := mustReadBytes(t, fixture.targetPath); !bytes.Equal(got, targetBefore) {
		t.Fatalf("target changed: got %q, want %q", got, targetBefore)
	}
	if got := mustReadBytes(t, fixture.targetDLLPath); !bytes.Equal(got, dllBefore) {
		t.Fatalf("DLL changed: got %q, want %q", got, dllBefore)
	}
	for _, path := range []string{marker.BinaryBackupPath, marker.DLLBackupPath, markerPath(fixture.configPath), stageDir} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cleanup path %q stat error = %v, want not exists", path, err)
		}
	}
}

func TestApplyRollsBackWhenServiceStartFails(t *testing.T) {
	fixture := newApplyFixture(t, applyFixtureOptions{withExistingDLL: true})
	service := &stubServiceController{startErr: errors.New("boom")}

	err := applyWithDeps(context.Background(), fixture.request, applyDeps{
		waitForParent: func(context.Context, int) error { return nil },
		service:       service,
		sleep:         testSleep,
		pollInterval:  time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "start updated service") {
		t.Fatalf("applyWithDeps() error = %v, want start failure", err)
	}

	if got := mustReadString(t, fixture.targetPath); got != "old-binary" {
		t.Fatalf("target contents = %q, want restored old binary", got)
	}
	if got := mustReadString(t, fixture.targetDLLPath); got != "old-dll" {
		t.Fatalf("target dll contents = %q, want restored old dll", got)
	}
	if service.startCalls != 2 {
		t.Fatalf("start calls = %d, want 2 (new + rollback)", service.startCalls)
	}
	if service.stopCalls != 2 {
		t.Fatalf("stop calls = %d, want 2 (initial + rollback)", service.stopCalls)
	}
	if _, err := os.Stat(markerPath(fixture.configPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker stat error = %v, want handled rollback cleanup", err)
	}
}

func TestApplyRollsBackAfterHealthTimeoutAndRemovesIntroducedDLL(t *testing.T) {
	fixture := newApplyFixture(t, applyFixtureOptions{withExistingDLL: false, healthTimeoutMillis: 25})
	service := &stubServiceController{
		startHook: func(string) error {
			return writeHealthMarker(markerPath(fixture.configPath), healthMarker{
				Nonce:   "wrong",
				Version: fixture.request.Version,
				State:   healthStateHealthy,
				Phase:   phaseHealthy,
			})
		},
	}

	err := applyWithDeps(context.Background(), fixture.request, applyDeps{
		waitForParent: func(context.Context, int) error { return nil },
		service:       service,
		sleep:         testSleep,
		pollInterval:  time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "healthy") {
		t.Fatalf("applyWithDeps() error = %v, want health timeout", err)
	}

	if got := mustReadString(t, fixture.targetPath); got != "old-binary" {
		t.Fatalf("target contents = %q, want restored old binary", got)
	}
	if _, err := os.Stat(fixture.targetDLLPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target dll stat error = %v, want not exists", err)
	}
	if service.startCalls != 2 {
		t.Fatalf("start calls = %d, want 2 (new + rollback)", service.startCalls)
	}
	if service.stopCalls != 2 {
		t.Fatalf("stop calls = %d, want 2 (initial + rollback)", service.stopCalls)
	}
}

func TestApplyStopsServiceBeforeParentWaitAndReplacement(t *testing.T) {
	fixture := newApplyFixture(t, applyFixtureOptions{withExistingDLL: true})
	var events []string
	service := &stubServiceController{
		stopHook: func(string) error {
			events = append(events, "stop")
			return nil
		},
		startHook: func(configPath string) error {
			events = append(events, "start")
			return MarkHealthy(configPath, fixture.request.Version)
		},
	}

	err := applyWithDeps(context.Background(), fixture.request, applyDeps{
		waitForParent: func(context.Context, int) error {
			events = append(events, "wait")
			return nil
		},
		service:      service,
		sleep:        testSleep,
		pollInterval: time.Millisecond,
		afterPhase: func(phase updatePhase) error {
			switch phase {
			case phaseBackupsReady:
				events = append(events, "backup")
			case phaseBinaryReplaced:
				events = append(events, "replace")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("applyWithDeps() error = %v", err)
	}
	want := []string{"stop", "wait", "backup", "replace", "start"}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if service.startCalls != 1 {
		t.Fatalf("start calls = %d, want exactly 1", service.startCalls)
	}
}

func TestApplyStopFailureLeavesInstalledStateUntouched(t *testing.T) {
	fixture := newApplyFixture(t, applyFixtureOptions{withExistingDLL: true})
	fixture.request.StageDir = filepath.Dir(fixture.stagedBinary)
	fixture.request.RequestPath = filepath.Join(fixture.request.StageDir, "request.json")
	fixture.request.HelperPath = filepath.Join(fixture.request.StageDir, "helper")
	writeFileWithMode(t, fixture.request.RequestPath, []byte("{}"), 0o600)
	writeFileWithMode(t, fixture.request.HelperPath, []byte("helper"), 0o700)

	targetBefore := mustReadBytes(t, fixture.targetPath)
	dllBefore := mustReadBytes(t, fixture.targetDLLPath)
	configBefore := mustReadBytes(t, fixture.configPath)
	paymentsBefore := mustReadBytes(t, fixture.paymentsPath)
	waitCalled := false
	service := &stubServiceController{stopErr: errors.New("stop failed")}

	err := applyWithDeps(context.Background(), fixture.request, applyDeps{
		waitForParent: func(context.Context, int) error {
			waitCalled = true
			return nil
		},
		service:      service,
		sleep:        testSleep,
		pollInterval: time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "stop service before update") {
		t.Fatalf("applyWithDeps() error = %v, want stop failure", err)
	}
	if waitCalled {
		t.Fatal("parent wait called after service stop failure")
	}
	if service.startCalls != 0 {
		t.Fatalf("start calls = %d, want 0", service.startCalls)
	}
	for path, want := range map[string][]byte{
		fixture.targetPath:    targetBefore,
		fixture.targetDLLPath: dllBefore,
		fixture.configPath:    configBefore,
		fixture.paymentsPath:  paymentsBefore,
	} {
		if got := mustReadBytes(t, path); !bytes.Equal(got, want) {
			t.Fatalf("%s changed: got %q, want %q", path, got, want)
		}
	}
	if _, err := os.Stat(markerPath(fixture.configPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker stat error = %v, want not exists", err)
	}
	if _, err := os.Stat(fixture.request.StageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage directory stat error = %v, want cleaned", err)
	}
}

func TestApplyWaitsForParentExitBeforeTouchingFiles(t *testing.T) {
	fixture := newApplyFixture(t, applyFixtureOptions{withExistingDLL: false})
	releaseParent := make(chan struct{})
	parentWaitStarted := make(chan struct{})
	service := &stubServiceController{
		startHook: func(configPath string) error {
			return MarkHealthy(configPath, fixture.request.Version)
		},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- applyWithDeps(context.Background(), fixture.request, applyDeps{
			waitForParent: func(context.Context, int) error {
				close(parentWaitStarted)
				<-releaseParent
				return nil
			},
			service:      service,
			sleep:        testSleep,
			pollInterval: time.Millisecond,
		})
	}()

	<-parentWaitStarted
	if got := mustReadString(t, fixture.targetPath); got != "old-binary" {
		t.Fatalf("target contents before parent exit = %q, want old binary", got)
	}
	if _, err := os.Stat(markerPath(fixture.configPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker stat error = %v, want not exists before parent exit", err)
	}
	if service.startCalls != 0 {
		t.Fatalf("start calls before parent exit = %d, want 0", service.startCalls)
	}
	if service.stopCalls != 1 {
		t.Fatalf("stop calls before parent exit = %d, want 1", service.stopCalls)
	}

	close(releaseParent)

	if err := <-errCh; err != nil {
		t.Fatalf("applyWithDeps() error = %v", err)
	}
}

func TestApplyParentWaitTimeoutLeavesFilesAndMarkerUntouched(t *testing.T) {
	fixture := newApplyFixture(t, applyFixtureOptions{withExistingDLL: true})
	targetBefore := mustReadBytes(t, fixture.targetPath)
	dllBefore := mustReadBytes(t, fixture.targetDLLPath)
	configBefore := mustReadBytes(t, fixture.configPath)
	paymentsBefore := mustReadBytes(t, fixture.paymentsPath)
	service := &stubServiceController{}

	err := applyWithDeps(context.Background(), fixture.request, applyDeps{
		waitForParent: func(ctx context.Context, _ int) error {
			<-ctx.Done()
			return ctx.Err()
		},
		service:      service,
		sleep:        testSleep,
		pollInterval: time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "wait for parent exit") {
		t.Fatalf("applyWithDeps() error = %v, want parent wait timeout", err)
	}
	if got := mustReadBytes(t, fixture.targetPath); string(got) != string(targetBefore) {
		t.Fatalf("target changed: got %q, want %q", got, targetBefore)
	}
	if got := mustReadBytes(t, fixture.targetDLLPath); !bytes.Equal(got, dllBefore) {
		t.Fatalf("DLL changed: got %q, want %q", got, dllBefore)
	}
	if got := mustReadBytes(t, fixture.configPath); !bytes.Equal(got, configBefore) {
		t.Fatalf("config changed: got %q, want %q", got, configBefore)
	}
	if got := mustReadBytes(t, fixture.paymentsPath); !bytes.Equal(got, paymentsBefore) {
		t.Fatalf("journal changed: got %q, want %q", got, paymentsBefore)
	}
	if _, err := os.Stat(fixture.stagedBinary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged binary stat error = %v, want cleanup", err)
	}
	if _, err := os.Stat(markerPath(fixture.configPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker stat error = %v, want not exists", err)
	}
	if service.stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", service.stopCalls)
	}
	if service.startCalls != 0 {
		t.Fatalf("start calls = %d, want 0 while parent may still be alive", service.startCalls)
	}
}

func TestApplyParentWaitFailurePreservesInterruptedRecoveryState(t *testing.T) {
	fixture := newApplyFixture(t, applyFixtureOptions{withExistingDLL: true})
	fixture.request.StageDir = filepath.Dir(fixture.stagedBinary)
	fixture.request.RequestPath = filepath.Join(fixture.request.StageDir, "request.json")
	fixture.request.HelperPath = filepath.Join(fixture.request.StageDir, "helper")
	writeFileWithMode(t, fixture.request.RequestPath, []byte("{}"), 0o600)
	writeFileWithMode(t, fixture.request.HelperPath, []byte("helper"), 0o700)
	if err := writePendingHealthMarker(fixture.request); err != nil {
		t.Fatalf("writePendingHealthMarker() error = %v", err)
	}
	marker, err := loadHealthMarker(markerPath(fixture.configPath))
	if err != nil {
		t.Fatalf("loadHealthMarker() error = %v", err)
	}
	if err := prepareBackups(&marker); err != nil {
		t.Fatalf("prepareBackups() error = %v", err)
	}
	marker.Phase = phaseBackupsReady
	if err := writeHealthMarker(markerPath(fixture.configPath), marker); err != nil {
		t.Fatalf("writeHealthMarker() error = %v", err)
	}
	targetBefore := mustReadBytes(t, fixture.targetPath)
	dllBefore := mustReadBytes(t, fixture.targetDLLPath)
	service := &stubServiceController{}

	err = applyWithDeps(context.Background(), fixture.request, applyDeps{
		waitForParent: func(context.Context, int) error { return errors.New("parent still alive") },
		service:       service,
		sleep:         testSleep,
		pollInterval:  time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "wait for parent exit") {
		t.Fatalf("applyWithDeps() error = %v, want parent wait failure", err)
	}
	if service.stopCalls != 1 || service.startCalls != 0 {
		t.Fatalf("service calls = stop %d, start %d; want stop 1, start 0", service.stopCalls, service.startCalls)
	}
	if got := mustReadBytes(t, fixture.targetPath); !bytes.Equal(got, targetBefore) {
		t.Fatalf("target changed: got %q, want %q", got, targetBefore)
	}
	if got := mustReadBytes(t, fixture.targetDLLPath); !bytes.Equal(got, dllBefore) {
		t.Fatalf("DLL changed: got %q, want %q", got, dllBefore)
	}
	for _, path := range []string{
		markerPath(fixture.configPath),
		marker.BinaryBackupPath,
		marker.DLLBackupPath,
		fixture.stagedBinary,
		fixture.stagedDLL,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("recovery artifact %q missing: %v", path, err)
		}
	}
	for _, path := range []string{fixture.request.RequestPath, fixture.request.HelperPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("transient artifact %q stat error = %v, want not exists", path, err)
		}
	}
}

func TestApplyResumesAfterInterruptedReplacementPhases(t *testing.T) {
	for _, faultPhase := range []updatePhase{
		phaseBackupsReady,
		phaseBinaryReplaced,
		phaseDLLReplaced,
	} {
		t.Run(string(faultPhase), func(t *testing.T) {
			fixture := newApplyFixture(t, applyFixtureOptions{withExistingDLL: true})
			service := &stubServiceController{
				startHook: func(configPath string) error {
					return MarkHealthy(configPath, fixture.request.Version)
				},
			}
			crash := errors.New("simulated crash")

			err := applyWithDeps(context.Background(), fixture.request, applyDeps{
				waitForParent: func(context.Context, int) error { return nil },
				service:       service,
				sleep:         testSleep,
				pollInterval:  time.Millisecond,
				afterPhase: func(phase updatePhase) error {
					if phase == faultPhase {
						return crash
					}
					return nil
				},
			})
			if !errors.Is(err, crash) {
				t.Fatalf("first apply error = %v, want simulated crash", err)
			}
			if _, err := os.Stat(fixture.targetPath); err != nil {
				t.Fatalf("target missing after %s: %v", faultPhase, err)
			}

			err = applyWithDeps(context.Background(), fixture.request, applyDeps{
				waitForParent: func(context.Context, int) error { return nil },
				service:       service,
				sleep:         testSleep,
				pollInterval:  time.Millisecond,
			})
			if err != nil {
				t.Fatalf("resume apply error = %v", err)
			}
			if got := mustReadString(t, fixture.targetPath); got != "new-binary" {
				t.Fatalf("target contents = %q, want new binary", got)
			}
			if got := mustReadString(t, fixture.targetDLLPath); got != "new-dll" {
				t.Fatalf("DLL contents = %q, want new DLL", got)
			}
		})
	}
}

func TestInterruptedUpdateVersionMismatchRetainsBackups(t *testing.T) {
	fixture := newApplyFixture(t, applyFixtureOptions{withExistingDLL: true})
	crash := errors.New("simulated crash")
	err := applyWithDeps(context.Background(), fixture.request, applyDeps{
		waitForParent: func(context.Context, int) error { return nil },
		service:       &stubServiceController{},
		sleep:         testSleep,
		pollInterval:  time.Millisecond,
		afterPhase: func(phase updatePhase) error {
			if phase == phaseBinaryReplaced {
				return crash
			}
			return nil
		},
	})
	if !errors.Is(err, crash) {
		t.Fatalf("apply error = %v, want simulated crash", err)
	}

	if err := MarkHealthy(fixture.configPath, "1.2.2"); err != nil {
		t.Fatalf("MarkHealthy() error = %v", err)
	}
	for _, path := range []string{
		backupPathFor(fixture.targetPath, fixture.request.Nonce),
		backupPathFor(fixture.targetDLLPath, fixture.request.Nonce),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("backup %q missing after mismatch: %v", path, err)
		}
	}
	marker, err := loadHealthMarker(markerPath(fixture.configPath))
	if err != nil {
		t.Fatalf("loadHealthMarker() error = %v", err)
	}
	if marker.State != healthStatePending {
		t.Fatalf("marker state = %q, want pending", marker.State)
	}
}

func TestApplyParentTimeoutCleansGeneratedArtifacts(t *testing.T) {
	fixture := newApplyFixture(t, applyFixtureOptions{withExistingDLL: true})
	fixture.request.RequestPath = filepath.Join(filepath.Dir(fixture.stagedBinary), "request.json")
	fixture.request.HelperPath = filepath.Join(filepath.Dir(fixture.stagedBinary), "helper")
	writeFileWithMode(t, fixture.request.RequestPath, []byte("{}"), 0o600)
	writeFileWithMode(t, fixture.request.HelperPath, []byte("helper"), 0o700)

	err := applyWithDeps(context.Background(), fixture.request, applyDeps{
		waitForParent: func(ctx context.Context, _ int) error {
			<-ctx.Done()
			return ctx.Err()
		},
		service:      &stubServiceController{},
		sleep:        testSleep,
		pollInterval: time.Millisecond,
	})
	if err == nil {
		t.Fatal("applyWithDeps() error = nil, want timeout")
	}
	if got := mustReadString(t, fixture.targetPath); got != "old-binary" {
		t.Fatalf("target contents = %q, want old binary", got)
	}
	for _, path := range []string{
		fixture.request.RequestPath,
		fixture.request.HelperPath,
		fixture.stagedBinary,
		fixture.stagedDLL,
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("artifact %q stat error = %v, want not exists", path, err)
		}
	}
}

func TestApplySuccessCleansGeneratedArtifactsAndStageDirectory(t *testing.T) {
	fixture := newApplyFixture(t, applyFixtureOptions{withExistingDLL: true})
	stageDir := filepath.Dir(fixture.stagedBinary)
	fixture.request.StageDir = stageDir
	fixture.request.RequestPath = filepath.Join(stageDir, "request.json")
	fixture.request.HelperPath = filepath.Join(stageDir, "helper")
	writeFileWithMode(t, fixture.request.RequestPath, []byte("{}"), 0o600)
	writeFileWithMode(t, fixture.request.HelperPath, []byte("helper"), 0o700)
	service := &stubServiceController{
		startHook: func(configPath string) error {
			return MarkHealthy(configPath, fixture.request.Version)
		},
	}

	err := applyWithDeps(context.Background(), fixture.request, applyDeps{
		waitForParent: func(context.Context, int) error { return nil },
		service:       service,
		sleep:         testSleep,
		pollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("applyWithDeps() error = %v", err)
	}
	if _, err := os.Stat(stageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage directory stat error = %v, want not exists", err)
	}
	if got := mustReadString(t, fixture.targetPath); got != "new-binary" {
		t.Fatalf("target contents = %q, want new binary", got)
	}
}

func TestLoadApplyRequestRejectsMalformedRequest(t *testing.T) {
	dir := t.TempDir()

	badJSONPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badJSONPath, []byte("{"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := LoadApplyRequest(badJSONPath); err == nil {
		t.Fatal("LoadApplyRequest() error = nil, want JSON error")
	}

	missingFieldPath := filepath.Join(dir, "missing.json")
	if err := os.WriteFile(missingFieldPath, []byte(`{"targetPath":"/tmp/target"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := LoadApplyRequest(missingFieldPath); err == nil {
		t.Fatal("LoadApplyRequest() error = nil, want validation error")
	}

	fixture := newApplyFixture(t, applyFixtureOptions{})
	for _, nonce := range []string{
		"../escape",
		"..",
		"abcd/efgh",
		"abcd\\efgh",
		"0123456789abcde.",
		"0123456789abcde\n",
		"short",
	} {
		req := fixture.request
		req.Nonce = nonce
		if err := req.Validate(); err == nil {
			t.Errorf("Validate() nonce %q error = nil, want error", nonce)
		}
	}
}

func TestMarkHealthyIgnoresMismatchedVersion(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := writeHealthMarker(markerPath(configPath), healthMarker{
		Nonce:   "nonce-1",
		Version: "1.2.3",
		State:   healthStatePending,
		Phase:   phaseDLLReplaced,
	}); err != nil {
		t.Fatalf("writeHealthMarker() error = %v", err)
	}

	if err := MarkHealthy(configPath, "2.0.0"); err != nil {
		t.Fatalf("MarkHealthy() error = %v", err)
	}
	marker, err := loadHealthMarker(markerPath(configPath))
	if err != nil {
		t.Fatalf("loadHealthMarker() error = %v", err)
	}
	if marker.State != healthStatePending {
		t.Fatalf("marker state = %q, want pending", marker.State)
	}

	if err := MarkHealthy(configPath, "1.2.3"); err != nil {
		t.Fatalf("MarkHealthy() error = %v", err)
	}
	marker, err = loadHealthMarker(markerPath(configPath))
	if err != nil {
		t.Fatalf("loadHealthMarker() error = %v", err)
	}
	if marker.State != healthStateHealthy {
		t.Fatalf("marker state = %q, want healthy", marker.State)
	}
}

func TestMarkHealthyReturnsMalformedMarkerError(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(markerPath(configPath), []byte("{"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := MarkHealthy(configPath, "1.2.3"); err == nil {
		t.Fatal("MarkHealthy() error = nil, want malformed marker error")
	}
}

type applyFixtureOptions struct {
	withExistingDLL     bool
	healthTimeoutMillis int
}

type applyFixture struct {
	configPath    string
	paymentsPath  string
	targetPath    string
	targetDLLPath string
	stagedBinary  string
	stagedDLL     string
	request       ApplyRequest
}

func newApplyFixture(t *testing.T, opts applyFixtureOptions) applyFixture {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	paymentsPath := configPath + ".payments.json"
	targetPath := filepath.Join(dir, "mirai-agent")
	targetDLLPath := filepath.Join(dir, "libusb-1.0.dll")
	stagedBinary := filepath.Join(dir, "staged", "mirai-agent.new")
	stagedDLL := filepath.Join(dir, "staged", "libusb-1.0.dll")
	if err := os.MkdirAll(filepath.Dir(stagedBinary), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(configPath, []byte("token = \"secret\"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.WriteFile(paymentsPath, []byte(`{"entries":[]}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	writeFileWithMode(t, targetPath, []byte("old-binary"), 0o751)
	writeFileWithMode(t, stagedBinary, []byte("new-binary"), 0o644)
	writeFileWithMode(t, stagedDLL, []byte("new-dll"), 0o644)
	if opts.withExistingDLL {
		writeFileWithMode(t, targetDLLPath, []byte("old-dll"), 0o644)
	}
	timeout := opts.healthTimeoutMillis
	if timeout == 0 {
		timeout = 200
	}

	return applyFixture{
		configPath:    configPath,
		paymentsPath:  paymentsPath,
		targetPath:    targetPath,
		targetDLLPath: targetDLLPath,
		stagedBinary:  stagedBinary,
		stagedDLL:     stagedDLL,
		request: ApplyRequest{
			TargetPath:              targetPath,
			StagedBinaryPath:        stagedBinary,
			StagedLibUSBPath:        stagedDLL,
			ConfigPath:              configPath,
			ParentPID:               4242,
			Version:                 "1.2.3",
			Nonce:                   "0123456789abcdef0123456789abcdef",
			ParentExitTimeoutMillis: 20,
			HealthTimeoutMillis:     timeout,
		},
	}
}

type stubServiceController struct {
	mu         sync.Mutex
	startCalls int
	stopCalls  int
	startErr   error
	startHook  func(string) error
	stopErr    error
	stopHook   func(string) error
}

func (s *stubServiceController) Start(configPath string) error {
	s.mu.Lock()
	s.startCalls++
	hook := s.startHook
	err := s.startErr
	s.startErr = nil
	s.mu.Unlock()
	if hook != nil {
		if hookErr := hook(configPath); hookErr != nil {
			return hookErr
		}
	}
	return err
}

func (s *stubServiceController) Stop(configPath string) error {
	s.mu.Lock()
	s.stopCalls++
	hook := s.stopHook
	err := s.stopErr
	s.mu.Unlock()
	if hook != nil {
		if hookErr := hook(configPath); hookErr != nil {
			return hookErr
		}
	}
	return err
}

func writeFileWithMode(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()

	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("Chmod(%q) error = %v", path, err)
		}
	}
}

func mustReadString(t *testing.T, path string) string {
	t.Helper()
	return string(mustReadBytes(t, path))
}

func mustReadBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return data
}

func filePerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	return info.Mode().Perm()
}

func testSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func TestWriteAndLoadApplyRequestRoundTrip(t *testing.T) {
	fixture := newApplyFixture(t, applyFixtureOptions{withExistingDLL: false})

	path, err := WriteApplyRequest(t.TempDir(), fixture.request)
	if err != nil {
		t.Fatalf("WriteApplyRequest() error = %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := filePerm(t, path); got != 0o600 {
			t.Fatalf("request mode = %v, want 0600", got)
		}
	}
	loaded, err := LoadApplyRequest(path)
	if err != nil {
		t.Fatalf("LoadApplyRequest() error = %v", err)
	}
	gotJSON, err := json.Marshal(loaded)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	wantJSON, err := json.Marshal(fixture.request)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("loaded request = %s, want %s", gotJSON, wantJSON)
	}
}
