//go:build linux

package printer

import (
	"context"
	"fmt"
	"os"

	"github.com/mirai-agent/escpos-agent/internal/config"
)

// devLPPrinter writes ESC/POS bytes to a character device such as /dev/usb/lp0.
type devLPPrinter struct {
	path string
	f    *os.File
}

func newDevLP(pc config.PrinterConfig) (Printer, error) {
	if pc.Path == "" {
		return nil, fmt.Errorf("dev_lp: path is required")
	}
	return &devLPPrinter{path: pc.Path}, nil
}

func (d *devLPPrinter) Open(ctx context.Context) error {
	f, err := os.OpenFile(d.path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", d.path, err)
	}
	d.f = f
	return nil
}

func (d *devLPPrinter) Write(p []byte) (int, error) {
	if d.f == nil {
		return 0, fmt.Errorf("dev_lp: not open")
	}
	return d.f.Write(p)
}

func (d *devLPPrinter) Close() error {
	if d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}
