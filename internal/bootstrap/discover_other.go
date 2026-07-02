//go:build !windows

package bootstrap

func discoverSpooler() []PrinterOption { return nil }
