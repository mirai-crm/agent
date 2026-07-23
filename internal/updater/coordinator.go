package updater

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"time"
)

const (
	// defaultUpdateHTTPTimeout bounds manifest and asset downloads.
	defaultUpdateHTTPTimeout = 5 * time.Minute
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
}

// NewCoordinator builds a Coordinator with real dependencies. restart is
// called only if the apply helper fails to launch after the manager has
// already begun draining, so the caller can bring the service back to a
// servable state (e.g. exit so the OS service manager, configured to always
// restart, relaunches the process and polling resumes with a fresh manager).
// restart must never block indefinitely.
func NewCoordinator(cfg CoordinatorConfig, log *slog.Logger, restart func()) *Coordinator {
	if restart == nil {
		restart = func() {}
	}
	return &Coordinator{
		cfg:     cfg,
		log:     log,
		restart: restart,
	}
}

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

	ticker := time.NewTicker(time.Duration(c.cfg.CheckIntervalHours) * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !c.attempt(ctx, manager) {
				return
			}
		}
	}
}

// attempt performs one check-and-maybe-update cycle. Downloads finish before
// draining. After drain, a launch failure requests a restart so the service
// does not stay drained.
func (c *Coordinator) attempt(ctx context.Context, manager DrainableManager) bool {
	client := &http.Client{Timeout: defaultUpdateHTTPTimeout}

	release, err := checkRelease(ctx, client, c.cfg.CurrentVersion, runtime.GOOS, runtime.GOARCH)
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

	target, err := os.Executable()
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		c.log.Warn("updater: resolve current executable failed; will retry next interval", "error", err.Error())
		return true
	}

	staged, err := stageRelease(ctx, client, *release, target)
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

	c.log.Info("updater: release staged; draining before apply", "version", release.Version)
	drained := manager.BeginDrain()
	select {
	case <-drained:
	case <-ctx.Done():
		c.cleanupStaged(staged)
		return false
	}

	c.log.Info("updater: drain complete; launching apply helper", "version", release.Version)
	if err := startDetachedCommand(staged.BinaryPath, target, c.cfg.ConfigPath, os.Getpid()); err != nil {
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
	if err := os.RemoveAll(staged.Dir); err != nil {
		c.log.Warn("updater: cleanup staged artifacts failed", "error", err.Error())
	}
}
