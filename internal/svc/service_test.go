package svc

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestRunWorkerSkipsUpdaterWhenStartUpdaterIsNil(t *testing.T) {
	runner := &fakeManager{run: func(_ context.Context, ready func()) error {
		ready()
		return nil
	}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		runWorker(context.Background(), testLogger(), func() (managerRunner, error) {
			return runner, nil
		}, nil)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runWorker with nil startUpdater blocked unexpectedly")
	}
}

func TestRunWorkerStartsUpdaterAfterReadyAndWaitsOnShutdown(t *testing.T) {
	runner := &fakeManager{
		run: func(ctx context.Context, ready func()) error {
			ready()
			<-ctx.Done()
			return nil
		},
	}

	updaterStarted := make(chan struct{})
	updaterFinished := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		runWorker(ctx, testLogger(), func() (managerRunner, error) {
			return runner, nil
		}, func(ctx context.Context, _ UpdateManager) {
			close(updaterStarted)
			<-ctx.Done()
			close(updaterFinished)
		})
	}()

	select {
	case <-updaterStarted:
	case <-time.After(time.Second):
		t.Fatal("updater was not started after manager readiness")
	}
	select {
	case <-updaterFinished:
		t.Fatal("updater finished before shutdown was requested")
	default:
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runWorker did not return after cancellation")
	}
	select {
	case <-updaterFinished:
	default:
		t.Fatal("runWorker returned before the updater goroutine finished (leaked shutdown)")
	}
}

func TestResolveStartUpdaterDisablesInInteractiveForeground(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	called := false
	real := func(context.Context, UpdateManager) { called = true }

	got := resolveStartUpdater(log, func() bool { return true }, real)

	if got != nil {
		t.Fatal("resolveStartUpdater() != nil for an interactive/foreground run")
	}
	logged := logs.String()
	if !strings.Contains(logged, "interactive") && !strings.Contains(logged, "foreground") {
		t.Fatalf("logs = %q, want a clear foreground/interactive disabled message", logged)
	}
	if !strings.Contains(logged, "disabled") {
		t.Fatalf("logs = %q, want it to say automatic apply is disabled", logged)
	}
	if called {
		t.Fatal("startUpdater invoked despite interactive/foreground run")
	}
}

func TestResolveStartUpdaterKeepsCallbackForServiceRuntime(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	real := func(context.Context, UpdateManager) {}

	got := resolveStartUpdater(log, func() bool { return false }, real)

	if got == nil {
		t.Fatal("resolveStartUpdater() = nil for an actual service run, want the callback preserved")
	}
	if logs.String() != "" {
		t.Fatalf("logs = %q, want no disabled-updater log noise for an actual service run", logs.String())
	}
}

func TestNewRestartFuncRequestsServiceRestartAndStopsWorker(t *testing.T) {
	restarted := make(chan struct{}, 2)
	stopped := make(chan struct{}, 2)

	restart := newRestartFunc(testLogger(), func() error {
		restarted <- struct{}{}
		return nil
	}, func() {
		stopped <- struct{}{}
	})
	restart()
	restart()

	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("restart was not requested")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("worker was not asked to stop")
	}
	select {
	case <-restarted:
		t.Fatal("restart was requested more than once")
	default:
	}
	select {
	case <-stopped:
		t.Fatal("worker stop was requested more than once")
	default:
	}
}

type fakeManager struct {
	run   func(context.Context, func()) error
	drain func() <-chan struct{}
}

func (f *fakeManager) RunReady(ctx context.Context, ready func()) error {
	return f.run(ctx, ready)
}

func (f *fakeManager) BeginDrain() <-chan struct{} {
	if f.drain != nil {
		return f.drain()
	}
	ch := make(chan struct{})
	close(ch)
	return ch
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
