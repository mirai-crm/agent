package updater

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"time"
)

const (
	// defaultUpdateHTTPTimeout bounds both the release-metadata request and
	// the archive/checksums download for one check-and-stage attempt.
	defaultUpdateHTTPTimeout = 5 * time.Minute
	// defaultParentExitTimeoutMillis bounds how long the apply helper waits
	// for this process to exit after the OS service is stopped.
	defaultParentExitTimeoutMillis = 30_000
	// defaultHealthTimeoutMillis bounds how long the apply helper waits for
	// the restarted service to report healthy before rolling back.
	defaultHealthTimeoutMillis = 120_000
)

// DrainableManager is the subset of worker.Manager a Coordinator needs to
// pause task admission before handing off to the apply helper. It is defined
// here, rather than imported from the worker package, so that the updater
// package does not need to depend on worker.
type DrainableManager interface {
	BeginDrain() <-chan struct{}
}

// CoordinatorConfig carries the update policy and identity a Coordinator
// needs. It is a plain struct (not config.Config) so this package does not
// need to import config.
type CoordinatorConfig struct {
	Enabled            bool
	CheckIntervalHours int
	CurrentVersion     string
	ConfigPath         string
}

// Coordinator polls GitHub for a newer stable release while the agent runs
// as an OS service and, on finding one, stages, drains, and launches the
// detached apply helper. Coordinator must only be run while the process is
// managed by the OS service framework (never for the interactive foreground
// `run` command): the caller is responsible for that gating.
type Coordinator struct {
	cfg     CoordinatorConfig
	log     *slog.Logger
	restart func()
	deps    coordinatorDeps
}

// NewCoordinator builds a Coordinator with real dependencies. restart is
// called only if the apply helper fails to launch after the manager has
// already begun draining, so the caller can bring the service back to a
// servable state (e.g. exit so the OS service manager, configured to always
// restart, relaunches the process and polling resumes with a fresh manager).
// restart must never block indefinitely.
func NewCoordinator(cfg CoordinatorConfig, log *slog.Logger, restart func()) *Coordinator {
	checker := Checker{}
	stager := Stager{}
	if restart == nil {
		restart = func() {}
	}
	return &Coordinator{
		cfg:     cfg,
		log:     log,
		restart: restart,
		deps: coordinatorDeps{
			newHTTPClient: func() *http.Client { return &http.Client{Timeout: defaultUpdateHTTPTimeout} },
			newTicker:     func(d time.Duration) coordinatorTicker { return realTicker{time.NewTicker(d)} },
			check: func(ctx context.Context, client *http.Client, currentVersion string) (*Release, error) {
				return checker.Check(ctx, client, currentVersion, runtime.GOOS, runtime.GOARCH)
			},
			stage:      stager.Stage,
			launch:     LaunchHelper,
			executable: os.Executable,
			nonce:      generateNonce,
			pid:        os.Getpid,
		},
	}
}

// coordinatorDeps are the Coordinator's overridable dependencies; tests
// replace individual fields with fakes to keep coordinator tests fast and
// deterministic, mirroring the applyDeps pattern used by Apply.
type coordinatorDeps struct {
	newHTTPClient func() *http.Client
	newTicker     func(time.Duration) coordinatorTicker
	check         func(ctx context.Context, client *http.Client, currentVersion string) (*Release, error)
	stage         func(ctx context.Context, client *http.Client, release Release, opts StageOptions) (*StageResult, error)
	launch        func(ApplyRequest) (LaunchResult, error)
	executable    func() (string, error)
	nonce         func() (string, error)
	pid           func() int
}

// coordinatorTicker abstracts time.Ticker so tests can drive checks on a
// manually-controlled channel instead of waiting on real wall-clock time.
type coordinatorTicker interface {
	C() <-chan time.Time
	Stop()
}

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }

// Run blocks, checking for an update immediately and then every
// CheckIntervalHours, until ctx is cancelled. Only one attempt ever runs at
// a time: attempts triggered by the ticker are processed strictly
// sequentially by this single loop.
func (c *Coordinator) Run(ctx context.Context, manager DrainableManager) {
	if !c.cfg.Enabled {
		c.log.Info("updater: disabled by config; automatic update checks will not run")
		return
	}
	if c.cfg.CurrentVersion == "dev" {
		c.log.Info("updater: dev build; automatic update checks will not run")
		return
	}
	if c.cfg.CheckIntervalHours < 1 {
		c.log.Warn("updater: check_interval_hours must be at least 1; automatic update checks will not run",
			"check_interval_hours", c.cfg.CheckIntervalHours)
		return
	}

	if !c.attempt(ctx, manager) {
		return
	}

	ticker := c.deps.newTicker(time.Duration(c.cfg.CheckIntervalHours) * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			if !c.attempt(ctx, manager) {
				return
			}
		}
	}
}

// attempt performs exactly one check-and-maybe-update cycle. Any failure
// before draining is logged and left for the next interval; the current
// service keeps polling as normal. Once BeginDrain has been called the
// update is irreversible from the manager's point of view, so every
// prerequisite (staged files, nonce, a validated apply request) is prepared
// first; only a helper-launch failure can occur after draining, and that
// triggers c.restart so the service does not stay drained forever. It returns
// false when the coordinator should stop permanently for this process
// lifetime: shutdown/cancellation or a successful helper handoff.
func (c *Coordinator) attempt(ctx context.Context, manager DrainableManager) bool {
	client := c.deps.newHTTPClient()

	release, err := c.deps.check(ctx, client, c.cfg.CurrentVersion)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		c.log.Warn("updater: check for update failed; will retry next interval", "error", err.Error())
		return true
	}
	if release == nil {
		return true
	}
	c.log.Info("updater: newer release found", "version", release.Version)

	target, err := c.deps.executable()
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		c.log.Warn("updater: resolve current executable failed; will retry next interval", "error", err.Error())
		return true
	}

	staged, err := c.deps.stage(ctx, client, *release, StageOptions{TargetPath: target})
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		c.log.Warn("updater: stage release failed; will retry next interval", "error", err.Error())
		return true
	}
	if ctx.Err() != nil {
		c.cleanupStaged(staged)
		return false
	}

	nonce, err := c.deps.nonce()
	if err != nil {
		c.log.Warn("updater: generate nonce failed; will retry next interval", "error", err.Error())
		c.cleanupStaged(staged)
		return true
	}

	req := ApplyRequest{
		TargetPath:              target,
		StagedBinaryPath:        staged.BinaryPath,
		StagedLibUSBPath:        staged.LibUSBPath,
		StageDir:                staged.Dir,
		ConfigPath:              c.cfg.ConfigPath,
		ParentPID:               c.deps.pid(),
		Version:                 release.Version,
		Nonce:                   nonce,
		ParentExitTimeoutMillis: defaultParentExitTimeoutMillis,
		HealthTimeoutMillis:     defaultHealthTimeoutMillis,
	}
	if err := req.Validate(); err != nil {
		c.log.Warn("updater: apply request invalid; will retry next interval", "error", err.Error())
		c.cleanupStaged(staged)
		return true
	}

	c.log.Info("updater: release staged; draining before apply", "version", release.Version)
	drained := manager.BeginDrain()
	select {
	case <-drained:
	case <-ctx.Done():
		c.cleanupStaged(staged)
		return false
	}

	c.log.Info("updater: drain complete; launching apply helper", "version", release.Version)
	if _, err := c.deps.launch(req); err != nil {
		c.log.Error("updater: launch apply helper failed after drain; restarting service", "error", err.Error())
		c.cleanupStaged(staged)
		c.restart()
		return false
	}
	c.log.Info("updater: apply helper launched", "version", release.Version)
	return false
}

func (c *Coordinator) cleanupStaged(staged *StageResult) {
	if staged == nil {
		return
	}
	req := ApplyRequest{StagedBinaryPath: staged.BinaryPath, StagedLibUSBPath: staged.LibUSBPath, StageDir: staged.Dir}
	if err := cleanupGeneratedArtifacts(req); err != nil {
		c.log.Warn("updater: cleanup staged artifacts failed", "error", err.Error())
	}
}

// generateNonce returns a cryptographically random, lowercase-hex nonce
// accepted by ApplyRequest.Validate.
func generateNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
