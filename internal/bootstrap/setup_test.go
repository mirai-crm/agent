package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/mirai-agent/mirai-agent/internal/api"
)

func TestSetupConfiguresLabelPrinter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device":{
			"id":9,
			"name":"Labels",
			"type":"label_printer",
			"registeredAt":"2026-07-20T00:00:00Z",
			"processedTasks":0,
			"queuedTasks":0
		}}`))
	}))
	defer server.Close()

	result, err := Run(context.Background(), Options{
		APIURL:       server.URL,
		Tokens:       []string{"label-token"},
		PrinterBinds: map[string]string{"9": "usb:0x1234:0x5678"},
		NoService:    true,
		Yes:          true,
		ConfigPath:   filepath.Join(t.TempDir(), "config.toml"),
	}, nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(result.Devices) != 1 {
		t.Fatalf("devices = %d", len(result.Devices))
	}
	dev := result.Devices[0]
	if dev.Type != api.DeviceTypeLabelPrinter || dev.Printer.Kind != "usb" {
		t.Fatalf("device = %+v", dev)
	}
	if dev.Label.DPI != 203 || dev.Label.GapMM != 2 {
		t.Fatalf("label config = %+v", dev.Label)
	}
}
