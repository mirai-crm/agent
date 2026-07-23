// Package svc wraps github.com/kardianos/service to install, uninstall and run
// the agent as an OS service (systemd / Windows SCM / launchd).
package svc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/kardianos/service"

	"github.com/mirai-agent/mirai-agent/internal/config"
	"github.com/mirai-agent/mirai-agent/internal/worker"
)

const (
	serviceName        = "MiraiAgent"
	serviceDisplayName = "CRM Printer and POS Agent"
	serviceDescription = "Polls the CRM task queue for receipt/label printing and direct-TCP POS terminal purchases."
)

// program implements service.Interface.
type program struct {
	cfg          config.Config
	configPath   string
	log          *slog.Logger
	startUpdater func(context.Context, UpdateManager)
	cancel       context.CancelFunc
	done         chan struct{}
}

type updaterStarter func(context.Context, UpdateManager, func())

type managerRunner interface {
	RunReady(context.Context, func()) error
	BeginDrain() <-chan struct{}
}

// UpdateManager is the subset of the worker Manager an update coordinator
// needs to pause task admission before an update is applied. It is declared
// here (rather than imported from the updater package) so this package does
// not depend on updater, which itself depends on svc to control the service
// during apply.
type UpdateManager interface {
	BeginDrain() <-chan struct{}
}

func (p *program) Start(s service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	startUpdater := resolveStartUpdater(p.log, nil, p.startUpdater)
	go func() {
		defer close(p.done)
		runWorker(ctx, p.log, func() (managerRunner, error) {
			return worker.NewManager(p.cfg, p.configPath, p.log)
		}, startUpdater)
	}()
	return nil
}

// resolveStartUpdater decides whether automatic update apply should run at
// all: only under an actual OS service, never for the interactive foreground
// `run` command. isInteractive defaults to kardianos's service.Interactive
// but is injectable so tests can force either branch deterministically. In
// the foreground/interactive case it logs clearly that automatic apply is
// disabled instead of silently doing nothing.
func resolveStartUpdater(log *slog.Logger, isInteractive func() bool, startUpdater func(context.Context, UpdateManager)) func(context.Context, UpdateManager) {
	if isInteractive == nil {
		isInteractive = service.Interactive
	}
	if isInteractive() {
		log.Info("running interactively in the foreground; automatic update apply is disabled (install and run as a service to enable it)")
		return nil
	}
	return startUpdater
}

func newRestartFunc(log *slog.Logger, requestRestart func() error, stopWorker func()) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			if requestRestart != nil {
				go func() {
					if err := requestRestart(); err != nil {
						log.Error("request service restart", "error", err.Error())
					}
				}()
			}
			if stopWorker != nil {
				stopWorker()
			}
		})
	}
}

func runWorker(ctx context.Context, log *slog.Logger, build func() (managerRunner, error), startUpdater func(context.Context, UpdateManager)) {
	mgr, err := build()
	if err != nil {
		log.Error("create worker manager", "error", err.Error())
		return
	}
	var updaterWG sync.WaitGroup
	ready := func() {
		if startUpdater != nil {
			updaterWG.Add(1)
			go func() {
				defer updaterWG.Done()
				startUpdater(ctx, mgr)
			}()
		}
	}
	runErr := mgr.RunReady(ctx, ready)
	updaterWG.Wait()
	if runErr != nil {
		log.Error("worker manager exited with error", "error", runErr.Error())
	}
}

func (p *program) Stop(s service.Service) error {
	p.log.Info("service stopping")
	if p.cancel != nil {
		p.cancel()
	}
	if p.done != nil {
		<-p.done
	}
	return nil
}

func serviceConfig(configPath string) *service.Config {
	return &service.Config{
		Name:        serviceName,
		DisplayName: serviceDisplayName,
		Description: serviceDescription,
		Arguments:   []string{"run", "--config", configPath},
		Option: service.KeyValue{
			// Auto-restart on failure (maps to systemd Restart=always / SCM failure actions).
			"Restart":           "always",
			"SuccessExitStatus": "0",
		},
	}
}

// newService builds a kardianos service bound to a program.
func newService(cfg config.Config, configPath string, log *slog.Logger) (service.Service, *program, error) {
	prg := &program{cfg: cfg, configPath: configPath, log: log}
	s, err := service.New(prg, serviceConfig(configPath))
	if err != nil {
		return nil, nil, err
	}
	return s, prg, nil
}

// Run runs the agent under the service manager (or in the foreground when not
// launched as a service). It blocks until stopped. startUpdater, if non-nil,
// is invoked once the worker manager is ready, but only when this process is
// actually managed by the OS service framework; it is never invoked for an
// interactive foreground run (see resolveStartUpdater).
func Run(cfg config.Config, configPath string, log *slog.Logger, startUpdater updaterStarter) error {
	s, prg, err := newService(cfg, configPath, log)
	if err != nil {
		return err
	}
	if startUpdater != nil {
		restart := newRestartFunc(log, s.Restart, func() {
			if prg.cancel != nil {
				prg.cancel()
			}
		})
		prg.startUpdater = func(ctx context.Context, mgr UpdateManager) {
			startUpdater(ctx, mgr, restart)
		}
	}
	return s.Run()
}

// Install registers and starts the OS service.
func Install(configPath string, log *slog.Logger) error {
	// Config isn't needed to install; pass an empty one.
	s, _, err := newService(config.Config{}, configPath, log)
	if err != nil {
		return err
	}
	if err := s.Install(); err != nil {
		return wrapPermErr(err)
	}
	if err := s.Start(); err != nil {
		return wrapPermErr(err)
	}
	return nil
}

// Start starts the already-registered OS service for the provided config.
func Start(configPath string) error {
	s, _, err := newService(config.Config{}, configPath, slog.Default())
	if err != nil {
		return err
	}
	if err := s.Start(); err != nil {
		return wrapPermErr(err)
	}
	return nil
}

// Stop stops the already-registered OS service for the provided config.
func Stop(configPath string) error {
	s, _, err := newService(config.Config{}, configPath, slog.Default())
	if err != nil {
		return err
	}
	if err := s.Stop(); err != nil {
		return wrapPermErr(err)
	}
	return nil
}

// Uninstall stops and removes the OS service.
func Uninstall(configPath string, log *slog.Logger) error {
	s, _, err := newService(config.Config{}, configPath, log)
	if err != nil {
		return err
	}
	// Best-effort stop before removal.
	_ = s.Stop()
	if err := s.Uninstall(); err != nil {
		return wrapPermErr(err)
	}
	return nil
}

// Status returns a human-readable service status string.
func Status(configPath string) (string, error) {
	s, _, err := newService(config.Config{}, configPath, slog.Default())
	if err != nil {
		return "", err
	}
	st, err := s.Status()
	if err != nil {
		return "unknown", err
	}
	switch st {
	case service.StatusRunning:
		return "running", nil
	case service.StatusStopped:
		return "stopped", nil
	default:
		return "unknown", nil
	}
}

// PermissionError signals that a privileged operation lacked rights (exit 5).
type PermissionError struct{ err error }

func (e *PermissionError) Error() string { return e.err.Error() }
func (e *PermissionError) Unwrap() error { return e.err }

func wrapPermErr(err error) error {
	if err == nil {
		return nil
	}
	if os.IsPermission(err) {
		return &PermissionError{err: err}
	}
	// kardianos surfaces permission problems as generic errors on some OSes.
	return fmt.Errorf("service operation failed (are you root/administrator?): %w", err)
}
