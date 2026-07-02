//go:build !linux

package printer

import "github.com/mirai-agent/mirai-agent/internal/config"

func newDevLP(pc config.PrinterConfig) (Printer, error) {
	return nil, notSupportedError(config.KindDevLP)
}
