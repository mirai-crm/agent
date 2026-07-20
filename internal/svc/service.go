// Package svc wraps github.com/kardianos/service to install, uninstall and run
// the agent as an OS service (systemd / Windows SCM / launchd).
package svc

import (
	"context"
	"fmt"
	"log/slog"
	"os"

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
	cfg        config.Config
	configPath string
	log        *slog.Logger
	cancel     context.CancelFunc
	done       chan struct{}
}

func (p *program) Start(s service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		mgr, err := worker.NewManager(p.cfg, p.configPath, p.log)
		if err != nil {
			p.log.Error("create worker manager", "error", err.Error())
			return
		}
		if err := mgr.Run(ctx); err != nil {
			p.log.Error("worker manager exited with error", "error", err.Error())
		}
	}()
	return nil
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
// launched as a service). It blocks until stopped.
func Run(cfg config.Config, configPath string, log *slog.Logger) error {
	s, _, err := newService(cfg, configPath, log)
	if err != nil {
		return err
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
