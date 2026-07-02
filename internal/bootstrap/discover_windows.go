//go:build windows

package bootstrap

import (
	"github.com/alexbrainman/printer"

	"github.com/mirai-agent/escpos-agent/internal/config"
)

func discoverSpooler() []PrinterOption {
	names, err := printer.ReadNames()
	if err != nil {
		return nil
	}
	var opts []PrinterOption
	for _, n := range names {
		opts = append(opts, PrinterOption{
			Label:  "windows_spooler: " + n,
			Config: config.PrinterConfig{Kind: config.KindWindowsSpooler, SpoolerName: n},
		})
	}
	return opts
}
