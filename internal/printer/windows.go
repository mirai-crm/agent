//go:build windows

package printer

import (
	"context"
	"fmt"

	"github.com/alexbrainman/printer"
	"github.com/mirai-agent/escpos-agent/internal/config"
)

// windowsSpooler prints raw ESC/POS bytes through the Windows spooler, bypassing
// the driver (RAW datatype).
type windowsSpooler struct {
	name string
	p    *printer.Printer
}

func newWindowsSpooler(pc config.PrinterConfig) (Printer, error) {
	if pc.SpoolerName == "" {
		return nil, fmt.Errorf("windows_spooler: spooler_name is required")
	}
	return &windowsSpooler{name: pc.SpoolerName}, nil
}

func (w *windowsSpooler) Open(ctx context.Context) error {
	p, err := printer.Open(w.name)
	if err != nil {
		return fmt.Errorf("open spooler %q: %w", w.name, err)
	}
	if err := p.StartRawDocument("escpos-agent receipt"); err != nil {
		p.Close()
		return fmt.Errorf("start raw document: %w", err)
	}
	w.p = p
	return nil
}

func (w *windowsSpooler) Write(p []byte) (int, error) {
	if w.p == nil {
		return 0, fmt.Errorf("windows_spooler: not open")
	}
	return w.p.Write(p)
}

func (w *windowsSpooler) Close() error {
	if w.p == nil {
		return nil
	}
	endErr := w.p.EndDocument()
	closeErr := w.p.Close()
	w.p = nil
	if endErr != nil {
		return endErr
	}
	return closeErr
}
