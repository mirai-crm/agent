package updater

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoordinatorSkipsWhenDisabled(t *testing.T) {
	c, manager, _ := newCoordinatorFixture(t, CoordinatorConfig{
		Enabled: false, CheckIntervalHours: 6, CurrentVersion: "1.0.0",
	})
	c.deps.check = failIfCheckCalled(t)

	cancel, done := runCoordinator(c, manager)
	defer cancel()
	waitDone(t, done, "disabled config")

	if got := manager.beginDrainCalls(); got != 0 {
		t.Fatalf("BeginDrain calls = %d, want 0", got)
	}
}

func TestCoordinatorSkipsForDevVersion(t *testing.T) {
	c, manager, _ := newCoordinatorFixture(t, CoordinatorConfig{
		Enabled: true, CheckIntervalHours: 6, CurrentVersion: "dev",
	})
	c.deps.check = failIfCheckCalled(t)

	cancel, done := runCoordinator(c, manager)
	defer cancel()
	waitDone(t, done, "dev build")

	if got := manager.beginDrainCalls(); got != 0 {
		t.Fatalf("BeginDrain calls = %d, want 0", got)
	}
}

func TestCoordinatorImmediateCheckFindsNoUpdate(t *testing.T) {
	c, manager, _ := newCoordinatorFixture(t, CoordinatorConfig{
		Enabled: true, CheckIntervalHours: 6, CurrentVersion: "1.0.0",
	})
	checked := make(chan struct{}, 1)
	c.deps.check = func(context.Context, *http.Client, string) (*Release, error) {
		checked <- struct{}{}
		return nil, nil
	}
	c.deps.stage = failIfStageCalled(t)

	cancel, done := runCoordinator(c, manager)
	defer func() {
		cancel()
		<-done
	}()

	select {
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("check was not called immediately")
	}
	if got := manager.beginDrainCalls(); got != 0 {
		t.Fatalf("BeginDrain calls = %d, want 0 when no update is available", got)
	}
}

func TestCoordinatorImmediateUpdateStagesDrainsThenLaunches(t *testing.T) {
	c, manager, _ := newCoordinatorFixture(t, CoordinatorConfig{
		Enabled: true, CheckIntervalHours: 6, CurrentVersion: "1.0.0",
	})
	manager.release()

	rec := &eventRecorder{}
	orderedManager := &orderRecordingManager{inner: manager, rec: rec}

	stageDir, binPath := newStagedFiles(t)
	release := &Release{Version: "1.1.0"}

	c.deps.check = func(context.Context, *http.Client, string) (*Release, error) {
		rec.add("check")
		return release, nil
	}
	c.deps.stage = func(context.Context, *http.Client, Release, StageOptions) (*StageResult, error) {
		rec.add("stage")
		return &StageResult{Dir: stageDir, BinaryPath: binPath}, nil
	}
	launched := make(chan ApplyRequest, 1)
	c.deps.launch = func(req ApplyRequest) (LaunchResult, error) {
		rec.add("launch")
		launched <- req
		return LaunchResult{}, nil
	}

	cancel, done := runCoordinator(c, orderedManager)
	defer func() {
		cancel()
		<-done
	}()

	var req ApplyRequest
	select {
	case req = <-launched:
	case <-time.After(time.Second):
		t.Fatal("launch was not called")
	}

	if req.Version != "1.1.0" {
		t.Fatalf("launch req.Version = %q, want 1.1.0", req.Version)
	}
	if req.StagedBinaryPath != binPath {
		t.Fatalf("launch req.StagedBinaryPath = %q, want %q", req.StagedBinaryPath, binPath)
	}
	if !filepath.IsAbs(req.ConfigPath) {
		t.Fatalf("launch req.ConfigPath = %q, want absolute", req.ConfigPath)
	}
	if !validNonce(req.Nonce) {
		t.Fatalf("launch req.Nonce = %q, want valid lowercase hex nonce", req.Nonce)
	}

	if want := []string{"check", "stage", "drain", "launch"}; !reflect.DeepEqual(rec.snapshot(), want) {
		t.Fatalf("event order = %v, want %v", rec.snapshot(), want)
	}
	if got := manager.beginDrainCalls(); got != 1 {
		t.Fatalf("BeginDrain calls = %d, want 1", got)
	}
}

func TestCoordinatorWaitsForActiveDrainBeforeLaunch(t *testing.T) {
	c, manager, _ := newCoordinatorFixture(t, CoordinatorConfig{
		Enabled: true, CheckIntervalHours: 6, CurrentVersion: "1.0.0",
	})
	// manager.ch stays open: simulates an active task still draining.

	stageDir, binPath := newStagedFiles(t)
	c.deps.check = onceThenNoUpdate(&Release{Version: "1.1.0"})
	c.deps.stage = func(context.Context, *http.Client, Release, StageOptions) (*StageResult, error) {
		return &StageResult{Dir: stageDir, BinaryPath: binPath}, nil
	}
	launchCalled := make(chan struct{}, 1)
	c.deps.launch = func(ApplyRequest) (LaunchResult, error) {
		launchCalled <- struct{}{}
		return LaunchResult{}, nil
	}

	cancel, done := runCoordinator(c, manager)
	defer func() {
		cancel()
		<-done
	}()

	select {
	case <-manager.began:
	case <-time.After(time.Second):
		t.Fatal("BeginDrain was not called")
	}
	// The coordinator is now blocked selecting on the (unclosed) drain
	// channel, so launch cannot have run yet -- this is causally guaranteed
	// by the coordinator's control flow, not a timing race.
	select {
	case <-launchCalled:
		t.Fatal("launch called before drain completed")
	default:
	}

	manager.release()

	select {
	case <-launchCalled:
	case <-time.After(time.Second):
		t.Fatal("launch was not called after drain completed")
	}
}

func TestCoordinatorPreDrainFailureRetriesNextInterval(t *testing.T) {
	c, manager, mt := newCoordinatorFixture(t, CoordinatorConfig{
		Enabled: true, CheckIntervalHours: 6, CurrentVersion: "1.0.0",
	})
	manager.release()

	release := &Release{Version: "1.1.0"}
	c.deps.check = func(context.Context, *http.Client, string) (*Release, error) {
		return release, nil
	}

	stageDir, binPath := newStagedFiles(t)
	var stageCalls int32
	c.deps.stage = func(context.Context, *http.Client, Release, StageOptions) (*StageResult, error) {
		if atomic.AddInt32(&stageCalls, 1) == 1 {
			return nil, errors.New("download failed")
		}
		return &StageResult{Dir: stageDir, BinaryPath: binPath}, nil
	}
	launchCalled := make(chan struct{}, 1)
	c.deps.launch = func(ApplyRequest) (LaunchResult, error) {
		launchCalled <- struct{}{}
		return LaunchResult{}, nil
	}

	cancel, done := runCoordinator(c, manager)
	defer func() {
		cancel()
		<-done
	}()

	mt.tick() // second interval: staging now succeeds.

	select {
	case <-launchCalled:
	case <-time.After(time.Second):
		t.Fatal("second attempt did not reach launch")
	}

	if got := atomic.LoadInt32(&stageCalls); got != 2 {
		t.Fatalf("stage calls = %d, want 2", got)
	}
	if got := manager.beginDrainCalls(); got != 1 {
		t.Fatalf("BeginDrain calls = %d, want 1 (never drains on a pre-drain failure)", got)
	}
}

func TestCoordinatorPostDrainLaunchFailureRequestsRestartAndCleansArtifacts(t *testing.T) {
	c, manager, _ := newCoordinatorFixture(t, CoordinatorConfig{
		Enabled: true, CheckIntervalHours: 6, CurrentVersion: "1.0.0",
	})
	manager.release()

	stageDir, binPath := newStagedFiles(t)
	c.deps.check = onceThenNoUpdate(&Release{Version: "1.1.0"})
	c.deps.stage = func(context.Context, *http.Client, Release, StageOptions) (*StageResult, error) {
		return &StageResult{Dir: stageDir, BinaryPath: binPath}, nil
	}
	c.deps.launch = func(ApplyRequest) (LaunchResult, error) {
		return LaunchResult{}, errors.New("launch failed")
	}
	restarted := make(chan struct{}, 1)
	c.restart = func() { restarted <- struct{}{} }

	cancel, done := runCoordinator(c, manager)
	defer func() {
		cancel()
		<-done
	}()

	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("restart was not requested after a post-drain launch failure")
	}

	if _, err := os.Stat(binPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged binary not cleaned up: stat err = %v", err)
	}
	if _, err := os.Stat(stageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged dir not cleaned up: stat err = %v", err)
	}
	if got := manager.beginDrainCalls(); got != 1 {
		t.Fatalf("BeginDrain calls = %d, want 1", got)
	}
}

func TestCoordinatorSuccessfulLaunchStopsFurtherChecks(t *testing.T) {
	c, manager, _ := newCoordinatorFixture(t, CoordinatorConfig{
		Enabled: true, CheckIntervalHours: 6, CurrentVersion: "1.0.0",
	})
	manager.release()

	stageDir, binPath := newStagedFiles(t)
	c.deps.check = onceThenNoUpdate(&Release{Version: "1.1.0"})
	c.deps.stage = func(context.Context, *http.Client, Release, StageOptions) (*StageResult, error) {
		return &StageResult{Dir: stageDir, BinaryPath: binPath}, nil
	}
	launched := make(chan struct{}, 1)
	c.deps.launch = func(ApplyRequest) (LaunchResult, error) {
		launched <- struct{}{}
		return LaunchResult{}, nil
	}

	cancel, done := runCoordinator(c, manager)
	defer cancel()

	select {
	case <-launched:
	case <-time.After(time.Second):
		t.Fatal("launch was not called")
	}
	waitDone(t, done, "successful helper launch")
}

func TestCoordinatorCancellationDuringDrainWaitReturnsWithoutLaunch(t *testing.T) {
	c, manager, _ := newCoordinatorFixture(t, CoordinatorConfig{
		Enabled: true, CheckIntervalHours: 6, CurrentVersion: "1.0.0",
	})
	// manager never released: the drain wait blocks until cancellation.

	stageDir, binPath := newStagedFiles(t)
	c.deps.check = onceThenNoUpdate(&Release{Version: "1.1.0"})
	c.deps.stage = func(context.Context, *http.Client, Release, StageOptions) (*StageResult, error) {
		return &StageResult{Dir: stageDir, BinaryPath: binPath}, nil
	}
	launchCalled := make(chan struct{}, 1)
	c.deps.launch = func(ApplyRequest) (LaunchResult, error) {
		launchCalled <- struct{}{}
		return LaunchResult{}, nil
	}

	cancel, done := runCoordinator(c, manager)

	select {
	case <-manager.began:
	case <-time.After(time.Second):
		t.Fatal("BeginDrain was not called")
	}

	cancel()
	waitDone(t, done, "cancellation during drain wait")

	select {
	case <-launchCalled:
		t.Fatal("launch called despite cancellation during drain wait")
	default:
	}
	if _, err := os.Stat(binPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged binary not cleaned up after cancellation: stat err = %v", err)
	}
	if _, err := os.Stat(stageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage dir not cleaned up after cancellation: stat err = %v", err)
	}
}

func TestCoordinatorDoesNotOverlapAttempts(t *testing.T) {
	c, manager, mt := newCoordinatorFixture(t, CoordinatorConfig{
		Enabled: true, CheckIntervalHours: 6, CurrentVersion: "1.0.0",
	})
	manager.release()
	c.deps.stage = failIfStageCalled(t)

	calls := make(chan int, 8)
	unblock := make(chan struct{})
	var mu sync.Mutex
	callNum := 0
	c.deps.check = func(context.Context, *http.Client, string) (*Release, error) {
		mu.Lock()
		callNum++
		n := callNum
		mu.Unlock()
		calls <- n
		if n == 2 {
			<-unblock
		}
		return nil, nil
	}

	cancel, done := runCoordinator(c, manager)
	defer func() {
		cancel()
		<-done
	}()

	if n := recvInt(t, calls); n != 1 {
		t.Fatalf("first call number = %d, want 1 (immediate check)", n)
	}

	mt.tick()
	if n := recvInt(t, calls); n != 2 {
		t.Fatalf("second call number = %d, want 2", n)
	}
	// Call 2 is now blocked inside check(). Queue up more ticks while blocked.
	mt.tick()
	mt.tick()

	select {
	case n := <-calls:
		t.Fatalf("unexpected overlapping check call %d while call 2 is still blocked", n)
	case <-time.After(100 * time.Millisecond):
	}

	close(unblock)

	if n := recvInt(t, calls); n != 3 {
		t.Fatalf("third call number = %d, want 3", n)
	}
	if n := recvInt(t, calls); n != 4 {
		t.Fatalf("fourth call number = %d, want 4", n)
	}
}

func TestGenerateNonceProducesValidLowercaseHex(t *testing.T) {
	nonce, err := generateNonce()
	if err != nil {
		t.Fatalf("generateNonce() error = %v", err)
	}
	if !validNonce(nonce) {
		t.Fatalf("generateNonce() = %q, want valid lowercase hex nonce", nonce)
	}
	other, err := generateNonce()
	if err != nil {
		t.Fatalf("generateNonce() error = %v", err)
	}
	if nonce == other {
		t.Fatalf("generateNonce() produced repeated nonce %q", nonce)
	}
}

// ---- fixtures and fakes ----

func newCoordinatorFixture(t *testing.T, cfg CoordinatorConfig) (*Coordinator, *fakeDrainManager, *manualTicker) {
	t.Helper()
	if cfg.ConfigPath == "" {
		cfg.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
	}
	manager := newFakeDrainManager()
	mt := newManualTicker()
	nonceN := 0
	c := &Coordinator{
		cfg:     cfg,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		restart: func() {},
		deps: coordinatorDeps{
			newHTTPClient: func() *http.Client { return &http.Client{} },
			newTicker:     func(time.Duration) coordinatorTicker { return mt },
			check:         func(context.Context, *http.Client, string) (*Release, error) { return nil, nil },
			stage: func(context.Context, *http.Client, Release, StageOptions) (*StageResult, error) {
				return nil, errors.New("stage not configured in fixture")
			},
			launch: func(ApplyRequest) (LaunchResult, error) {
				return LaunchResult{}, errors.New("launch not configured in fixture")
			},
			executable: func() (string, error) { return filepath.Join(t.TempDir(), "mirai-agent"), nil },
			nonce: func() (string, error) {
				nonceN++
				return generateNonce()
			},
			pid: func() int { return 4242 },
		},
	}
	return c, manager, mt
}

func runCoordinator(c *Coordinator, manager DrainableManager) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, manager)
	}()
	return cancel, done
}

func waitDone(t *testing.T, done <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Run() did not return for %s", label)
	}
}

func recvInt(t *testing.T, ch <-chan int) int {
	t.Helper()
	select {
	case n := <-ch:
		return n
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for check call")
		return -1
	}
}

func newStagedFiles(t *testing.T) (dir, binPath string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	binPath = filepath.Join(dir, "mirai-agent.new")
	if err := os.WriteFile(binPath, []byte("staged-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return dir, binPath
}

func onceThenNoUpdate(release *Release) func(context.Context, *http.Client, string) (*Release, error) {
	var called int32
	return func(context.Context, *http.Client, string) (*Release, error) {
		if atomic.AddInt32(&called, 1) == 1 {
			return release, nil
		}
		return nil, nil
	}
}

func failIfCheckCalled(t *testing.T) func(context.Context, *http.Client, string) (*Release, error) {
	return func(context.Context, *http.Client, string) (*Release, error) {
		t.Error("check should not be called")
		return nil, nil
	}
}

func failIfStageCalled(t *testing.T) func(context.Context, *http.Client, Release, StageOptions) (*StageResult, error) {
	return func(context.Context, *http.Client, Release, StageOptions) (*StageResult, error) {
		t.Error("stage should not be called")
		return nil, errors.New("stage should not be called")
	}
}

type eventRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *eventRecorder) add(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *eventRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// orderRecordingManager wraps a DrainableManager to record when BeginDrain is
// called relative to other coordinator steps.
type orderRecordingManager struct {
	inner DrainableManager
	rec   *eventRecorder
}

func (m *orderRecordingManager) BeginDrain() <-chan struct{} {
	m.rec.add("drain")
	return m.inner.BeginDrain()
}

// fakeDrainManager simulates worker.Manager's drain gate deterministically.
type fakeDrainManager struct {
	mu        sync.Mutex
	calls     int
	ch        chan struct{}
	began     chan struct{}
	beganOnce sync.Once
}

func newFakeDrainManager() *fakeDrainManager {
	return &fakeDrainManager{ch: make(chan struct{}), began: make(chan struct{})}
}

func (m *fakeDrainManager) BeginDrain() <-chan struct{} {
	m.mu.Lock()
	m.calls++
	ch := m.ch
	m.mu.Unlock()
	m.beganOnce.Do(func() { close(m.began) })
	return ch
}

func (m *fakeDrainManager) beginDrainCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// release marks the manager idle, as if all active task admissions finished.
func (m *fakeDrainManager) release() {
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-m.ch:
	default:
		close(m.ch)
	}
}

// manualTicker gives tests full control over when the coordinator's periodic
// check fires, without waiting on real wall-clock time.
type manualTicker struct {
	ch       chan time.Time
	stopOnce sync.Once
	stopped  chan struct{}
}

func newManualTicker() *manualTicker {
	return &manualTicker{ch: make(chan time.Time, 8), stopped: make(chan struct{})}
}

func (m *manualTicker) C() <-chan time.Time { return m.ch }
func (m *manualTicker) Stop()               { m.stopOnce.Do(func() { close(m.stopped) }) }
func (m *manualTicker) tick()               { m.ch <- time.Now() }
