//go:build cgo

package printer

import (
	"context"
	"fmt"
	"runtime"

	"github.com/google/gousb"
)

// usbPrinter writes raw command bytes directly to a printer's bulk-OUT endpoint.
type usbPrinter struct {
	vid, pid uint16
	serial   string

	ctx  *gousb.Context
	dev  *gousb.Device
	intf *gousb.Interface
	done func()
	ep   *gousb.OutEndpoint
}

func newUSB(vid, pid uint16, serial string) (Printer, error) {
	return &usbPrinter{vid: vid, pid: pid, serial: serial}, nil
}

func (u *usbPrinter) Open(ctx context.Context) error {
	u.ctx = gousb.NewContext()

	var dev *gousb.Device
	var err error
	if u.serial != "" {
		devs, oerr := u.ctx.OpenDevices(func(desc *gousb.DeviceDesc) bool {
			return desc.Vendor == gousb.ID(u.vid) && desc.Product == gousb.ID(u.pid)
		})
		if oerr != nil {
			u.cleanup()
			return fmt.Errorf("usb open devices: %w", oerr)
		}
		for _, d := range devs {
			s, _ := d.SerialNumber()
			if s == u.serial {
				dev = d
			} else {
				d.Close()
			}
		}
		if dev == nil {
			u.cleanup()
			return fmt.Errorf("usb device %04x:%04x serial %q not found", u.vid, u.pid, u.serial)
		}
	} else {
		dev, err = u.ctx.OpenDeviceWithVIDPID(gousb.ID(u.vid), gousb.ID(u.pid))
		if err != nil {
			u.cleanup()
			return fmt.Errorf("usb open %04x:%04x: %w", u.vid, u.pid, err)
		}
		if dev == nil {
			u.cleanup()
			return fmt.Errorf("usb device %04x:%04x not found", u.vid, u.pid)
		}
	}
	u.dev = dev

	// On Linux the kernel usblp driver typically claims the device; detach it.
	if runtime.GOOS == "linux" {
		if err := u.dev.SetAutoDetach(true); err != nil {
			u.cleanup()
			return fmt.Errorf("usb set auto detach: %w", err)
		}
	}

	intf, done, err := u.dev.DefaultInterface()
	if err != nil {
		u.cleanup()
		return fmt.Errorf("usb claim interface: %w", err)
	}
	u.intf = intf
	u.done = done

	epNum, err := firstBulkOutEndpoint(intf)
	if err != nil {
		u.cleanup()
		return err
	}
	ep, err := intf.OutEndpoint(epNum)
	if err != nil {
		u.cleanup()
		return fmt.Errorf("usb out endpoint %d: %w", epNum, err)
	}
	u.ep = ep
	return nil
}

func firstBulkOutEndpoint(intf *gousb.Interface) (int, error) {
	for _, ep := range intf.Setting.Endpoints {
		if ep.Direction == gousb.EndpointDirectionOut && ep.TransferType == gousb.TransferTypeBulk {
			return ep.Number, nil
		}
	}
	return 0, fmt.Errorf("usb: no bulk OUT endpoint on interface")
}

func (u *usbPrinter) Write(p []byte) (int, error) {
	if u.ep == nil {
		return 0, fmt.Errorf("usb: not open")
	}
	return u.ep.Write(p)
}

func (u *usbPrinter) Close() error {
	u.cleanup()
	return nil
}

func (u *usbPrinter) cleanup() {
	if u.done != nil {
		u.done()
		u.done = nil
	}
	u.intf = nil
	u.ep = nil
	if u.dev != nil {
		u.dev.Close()
		u.dev = nil
	}
	if u.ctx != nil {
		u.ctx.Close()
		u.ctx = nil
	}
}
