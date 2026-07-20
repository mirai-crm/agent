package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadLabelPrinterDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	const source = `
[server]
base_url = "https://crm.example.com"

[[devices]]
token = "label-token"
id = 9
name = "Labels"
type = "label_printer"

  [devices.printer]
  kind = "usb"
  vendor_id = "0x1234"
  product_id = "0x5678"
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	dev := cfg.Devices[0]
	if dev.WidthDots != 0 {
		t.Fatalf("WidthDots = %d, want 0", dev.WidthDots)
	}
	if dev.Label.DPI != 203 || dev.Label.GapMM != 2 || dev.Label.GapOffsetMM != 0 {
		t.Fatalf("Label defaults = %+v", dev.Label)
	}
}

func TestLabelPrinterRejectsInvalidDPI(t *testing.T) {
	cfg := Default()
	cfg.Server.BaseURL = "https://crm.example.com"
	cfg.Devices = []DeviceConfig{{
		Token: "label-token",
		ID:    9,
		Name:  "Labels",
		Type:  "label_printer",
		Printer: PrinterConfig{
			Kind:      KindUSB,
			VendorID:  "0x1234",
			ProductID: "0x5678",
		},
		Label: LabelConfig{DPI: 600, GapMM: 2},
	}}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "label.dpi") {
		t.Fatalf("Validate() error = %v", err)
	}
}
