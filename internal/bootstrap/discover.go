package bootstrap

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mirai-agent/escpos-agent/internal/config"
)

// PrinterOption is a discovered candidate the operator can pick during setup.
type PrinterOption struct {
	Label  string
	Config config.PrinterConfig
}

// discoverPrinters returns candidate printer bindings for the current OS. USB
// devices are not auto-listed here (entered manually) to avoid requiring libusb
// during setup.
func discoverPrinters() []PrinterOption {
	var opts []PrinterOption

	// Windows spooler printers (build-tagged; no-op on other OSes).
	opts = append(opts, discoverSpooler()...)

	switch runtime.GOOS {
	case "linux":
		opts = append(opts, discoverDevLP()...)
		opts = append(opts, discoverCUPS()...)
	case "darwin":
		opts = append(opts, discoverCUPS()...)
	}
	return opts
}

// discoverDevLP lists /dev/usb/lp* character devices.
func discoverDevLP() []PrinterOption {
	matches, _ := filepath.Glob("/dev/usb/lp*")
	var opts []PrinterOption
	for _, m := range matches {
		opts = append(opts, PrinterOption{
			Label:  "dev_lp: " + m,
			Config: config.PrinterConfig{Kind: config.KindDevLP, Path: m},
		})
	}
	return opts
}

// discoverCUPS lists CUPS print queues via `lpstat -a`.
func discoverCUPS() []PrinterOption {
	out, err := exec.Command("lpstat", "-a").Output()
	if err != nil {
		return nil
	}
	var opts []PrinterOption
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		queue := fields[0]
		opts = append(opts, PrinterOption{
			Label:  "cups_raw: " + queue + " (ensure queue is raw)",
			Config: config.PrinterConfig{Kind: config.KindCUPSRaw, Queue: queue},
		})
	}
	return opts
}
