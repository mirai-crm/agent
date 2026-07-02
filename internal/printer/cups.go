//go:build linux || darwin

package printer

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"

	"github.com/mirai-agent/escpos-agent/internal/config"
)

// cupsRawPrinter buffers the ESC/POS job and pipes it to a CUPS raw queue via
// `lp -d <queue> -o raw`. Buffering keeps the whole job in one lp invocation,
// which is simplest and reliable for receipt-sized data.
type cupsRawPrinter struct {
	queue string
	buf   bytes.Buffer
}

func newCUPSRaw(pc config.PrinterConfig) (Printer, error) {
	if pc.Queue == "" {
		return nil, fmt.Errorf("cups_raw: queue is required")
	}
	return &cupsRawPrinter{queue: pc.Queue}, nil
}

func (c *cupsRawPrinter) Open(ctx context.Context) error {
	c.buf.Reset()
	return nil
}

func (c *cupsRawPrinter) Write(p []byte) (int, error) {
	return c.buf.Write(p)
}

func (c *cupsRawPrinter) Close() error {
	if c.buf.Len() == 0 {
		return nil
	}
	cmd := exec.Command("lp", "-d", c.queue, "-o", "raw")
	cmd.Stdin = bytes.NewReader(c.buf.Bytes())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("lp to queue %s: %w: %s", c.queue, err, stderr.String())
	}
	c.buf.Reset()
	return nil
}
