//go:build !cgo

package printer

import "fmt"

// newUSB is unavailable without cgo (gousb requires libusb). Build with
// CGO_ENABLED=1 and libusb installed to use direct USB printing.
func newUSB(vid, pid uint16, serial string) (Printer, error) {
	return nil, fmt.Errorf("usb printing requires a cgo build with libusb (CGO_ENABLED=1)")
}
