//go:build !(linux || darwin)

package printer

import "github.com/mirai-agent/escpos-agent/internal/config"

func newCUPSRaw(pc config.PrinterConfig) (Printer, error) {
	return nil, notSupportedError(config.KindCUPSRaw)
}
