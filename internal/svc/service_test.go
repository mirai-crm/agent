package svc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

func TestRunWorkerMarksHealthyAfterManagerConstructionBeforeRun(t *testing.T) {
	var events []string
	runner := managerRunnerFunc(func(context.Context) error {
		events = append(events, "run")
		return nil
	})

	runWorker(context.Background(), testLogger(), func() (managerRunner, error) {
		events = append(events, "construct")
		return runner, nil
	}, func() error {
		events = append(events, "healthy")
		return nil
	})

	if want := []string{"construct", "healthy", "run"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRunWorkerDoesNotMarkHealthyWhenManagerConstructionFails(t *testing.T) {
	marked := false

	runWorker(context.Background(), testLogger(), func() (managerRunner, error) {
		return nil, errors.New("build failed")
	}, func() error {
		marked = true
		return nil
	})

	if marked {
		t.Fatal("healthy marker written after manager construction failure")
	}
}

func TestRunWorkerContinuesWhenHealthMarkFails(t *testing.T) {
	ran := false
	var logs bytes.Buffer

	runWorker(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)), func() (managerRunner, error) {
		return managerRunnerFunc(func(context.Context) error {
			ran = true
			return nil
		}), nil
	}, func() error {
		return errors.New("marker unreadable")
	})

	if !ran {
		t.Fatal("manager did not run after health marker failure")
	}
	if !strings.Contains(logs.String(), "mark updater health") || !strings.Contains(logs.String(), "marker unreadable") {
		t.Fatalf("logs = %q, want health marker failure", logs.String())
	}
}

type managerRunnerFunc func(context.Context) error

func (f managerRunnerFunc) Run(ctx context.Context) error {
	return f(ctx)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
