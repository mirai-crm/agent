// Package printer provides a platform-independent sink for ESC/POS bytes and
// platform-specific implementations selected by config.
package printer

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mirai-agent/escpos-agent/internal/config"
)

// Printer is a platform-independent receiver of ESC/POS bytes.
type Printer interface {
	// Open prepares the device (opens file/USB/spooler job).
	Open(ctx context.Context) error
	// Write sends raw ESC/POS bytes; may be called multiple times (chunks).
	Write(p []byte) (int, error)
	// Close finishes the print job and releases resources.
	Close() error
}

// ChunkSize is the recommended maximum bytes per Write to a printer backend.
const ChunkSize = 8 << 10

// New builds a Printer from a device's printer config. The concrete
// constructors are provided per-platform via build tags; unsupported kinds on a
// given OS return a clear error.
func New(pc config.PrinterConfig) (Printer, error) {
	switch pc.Kind {
	case config.KindWindowsSpooler:
		return newWindowsSpooler(pc)
	case config.KindCUPSRaw:
		return newCUPSRaw(pc)
	case config.KindDevLP:
		return newDevLP(pc)
	case config.KindUSB:
		vid, err := parseHexID(pc.VendorID)
		if err != nil {
			return nil, fmt.Errorf("usb vendor_id: %w", err)
		}
		pid, err := parseHexID(pc.ProductID)
		if err != nil {
			return nil, fmt.Errorf("usb product_id: %w", err)
		}
		return newUSB(vid, pid, pc.Serial)
	default:
		return nil, fmt.Errorf("unknown printer kind %q", pc.Kind)
	}
}

// WriteChunked writes p to the printer in ChunkSize pieces.
func WriteChunked(p Printer, data []byte) error {
	for off := 0; off < len(data); off += ChunkSize {
		end := off + ChunkSize
		if end > len(data) {
			end = len(data)
		}
		if _, err := p.Write(data[off:end]); err != nil {
			return err
		}
	}
	return nil
}

// parseHexID parses "0x0416" or "0416"/"1046" (hex) into a uint16.
func parseHexID(s string) (uint16, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	v, err := strconv.ParseUint(s, 16, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid hex id %q: %w", s, err)
	}
	return uint16(v), nil
}

// notSupportedError is returned for kinds not available on the current OS/build.
func notSupportedError(kind string) error {
	return fmt.Errorf("printer kind %q is not supported on this platform/build", kind)
}
