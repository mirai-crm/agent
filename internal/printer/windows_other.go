//go:build !windows

package printer

import "github.com/mirai-agent/mirai-agent/internal/config"

func newWindowsSpooler(pc config.PrinterConfig) (Printer, error) {
	return nil, notSupportedError(config.KindWindowsSpooler)
}
