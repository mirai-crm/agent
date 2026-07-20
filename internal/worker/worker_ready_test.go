package worker

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/mirai-agent/mirai-agent/internal/api"
	"github.com/mirai-agent/mirai-agent/internal/config"
)

func TestManagerRunReadySignalsAfterValidWorkerBuiltAndLaunched(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.DeviceConfig{{
		ID:    81,
		Name:  "POS",
		Token: "token",
		Type:  api.DeviceTypePOSTerminal,
		POS: config.POSConfig{
			Address:                 "127.0.0.1:2000",
			ConnectTimeoutSeconds:   1,
			OperationTimeoutSeconds: 1,
			MerchantIDs:             map[string]string{"1111111111": "1"},
		},
	}}
	manager, err := NewManager(cfg, filepath.Join(t.TempDir(), "config.toml"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := false
	err = manager.RunReady(ctx, func() {
		ready = true
		cancel()
	})
	if err != nil {
		t.Fatalf("RunReady() error = %v", err)
	}
	if !ready {
		t.Fatal("readiness callback was not called")
	}
}

func TestManagerRunReadyRejectsAllInvalidWorkersWithoutReadiness(t *testing.T) {
	cfg := config.Default()
	cfg.Devices = []config.DeviceConfig{{
		ID:    1,
		Name:  "invalid",
		Token: "token",
		Type:  "unsupported",
	}}
	manager, err := NewManager(cfg, filepath.Join(t.TempDir(), "config.toml"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	ready := false
	err = manager.RunReady(context.Background(), func() { ready = true })
	if err == nil {
		t.Fatal("RunReady() error = nil, want no usable workers error")
	}
	if ready {
		t.Fatal("readiness callback was called")
	}
}
