package updater

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
	if service.stopCalls != 0 {
		t.Fatalf("stop calls = %d, want 0", service.stopCalls)
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
	if service.stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", service.stopCalls)
	}
	marker, err := loadHealthMarker(markerPath(fixture.configPath))
	if err != nil {
		t.Fatalf("loadHealthMarker() error = %v", err)
	}
	if marker.State != healthStatePending {
		t.Fatalf("marker state = %q, want pending", marker.State)
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
	if service.stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", service.stopCalls)
	}
}

func TestApplyWaitsForParentExitBeforeTouchingFiles(t *testing.T) {
	fixture := newApplyFixture(t, applyFixtureOptions{withExistingDLL: false})
	releaseParent := make(chan struct{})
	service := &stubServiceController{
		startHook: func(configPath string) error {
			return MarkHealthy(configPath, fixture.request.Version)
		},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- applyWithDeps(context.Background(), fixture.request, applyDeps{
			waitForParent: func(context.Context, int) error {
				<-releaseParent
				return nil
			},
			service:      service,
			sleep:        testSleep,
			pollInterval: time.Millisecond,
		})
	}()

	time.Sleep(10 * time.Millisecond)
	if got := mustReadString(t, fixture.targetPath); got != "old-binary" {
		t.Fatalf("target contents before parent exit = %q, want old binary", got)
	}
	if _, err := os.Stat(markerPath(fixture.configPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker stat error = %v, want not exists before parent exit", err)
	}
	if service.startCalls != 0 {
		t.Fatalf("start calls before parent exit = %d, want 0", service.startCalls)
	}

	close(releaseParent)

	if err := <-errCh; err != nil {
		t.Fatalf("applyWithDeps() error = %v", err)
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
}

func TestMarkHealthyIgnoresMismatchedVersion(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := writeHealthMarker(markerPath(configPath), healthMarker{
		Nonce:   "nonce-1",
		Version: "1.2.3",
		State:   healthStatePending,
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
			TargetPath:          targetPath,
			StagedBinaryPath:    stagedBinary,
			StagedLibUSBPath:    stagedDLL,
			ConfigPath:          configPath,
			ParentPID:           4242,
			Version:             "1.2.3",
			Nonce:               "nonce-123",
			HealthTimeoutMillis: timeout,
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
}

func (s *stubServiceController) Start(configPath string) error {
	s.mu.Lock()
	s.startCalls++
	hook := s.startHook
	err := s.startErr
	s.mu.Unlock()
	if hook != nil {
		if hookErr := hook(configPath); hookErr != nil {
			return hookErr
		}
	}
	return err
}

func (s *stubServiceController) Stop(string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopCalls++
	return s.stopErr
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
