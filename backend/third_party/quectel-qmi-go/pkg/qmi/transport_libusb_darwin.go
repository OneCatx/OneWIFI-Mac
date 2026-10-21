//go:build darwin && cgo

package qmi

/*
#cgo pkg-config: libusb-1.0
#include <stdlib.h>
#include <libusb.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unsafe"
)

func init() {
	// On macOS there is no /dev/cdc-wdmX. The only way to reach the QMI
	// channel of a USB-attached modem is through libusb bulk endpoints, so we
	// replace the raw-transport hook with a libusb-based implementation. The
	// "path" argument is a virtual identifier (e.g. "2ca3:4006") parsed by
	// parseLibusbPath below.
	openRawTransportHook = openLibusbRawTransport
}

const (
	qmiLibusbVendorDJI  = 0x2ca3
	qmiLibusbProductDJI = 0x4006
	qmiLibusbVendorQC   = 0x2c7c
	qmiLibusbProductQC  = 0x0125
)

// qmiDeviceIDs lists USB device IDs to probe for a QMI interface, in priority
// order. DJI modules may enumerate as 2ca3:4006 (DJI) or 2c7c:0125 (Quectel
// default) depending on configuration.
var qmiDeviceIDs = [][2]uint16{
	{qmiLibusbVendorDJI, qmiLibusbProductDJI},
	{qmiLibusbVendorQC, qmiLibusbProductQC},
}

// libusbQMITransport implements qmiTransport over libusb bulk endpoints.
//
// The QMI channel sits on a modem USB interface. Candidate filtering is
// device-aware so CTL_SYNC is not sent to unrelated diagnostics, AT, or ADB
// endpoints; a valid QMI CTL_SYNC round-trip is still required before use.
type libusbQMITransport struct {
	ctx         *C.libusb_context
	handle      *C.libusb_device_handle
	iface       int
	endpointIn  byte
	endpointOut byte

	// The QMI client runs a read loop and a writer loop concurrently; libusb
	// does not allow concurrent transfers on the same endpoint, so we guard
	// reads and writes with separate mutexes.
	readMu  sync.Mutex
	writeMu sync.Mutex

	// readDeadline is consulted by Read; SetReadDeadline is called by the
	// client read loop before every read.
	readDeadlineMu sync.Mutex
	readDeadline   time.Time

	closeMu sync.Mutex
	closed  bool
}

// openLibusbRawTransport opens the first USB modem that exposes a QMI bulk
// interface. The path is a virtual "VID:PID" identifier (e.g. "2ca3:4006");
// an empty path or "auto" probes the default DJI/Quectel ID list.
func openLibusbRawTransport(path string) (qmiTransport, error) {
	ids, err := parseLibusbPath(path)
	if err != nil {
		return nil, err
	}

	var ctx *C.libusb_context
	if rc := C.libusb_init(&ctx); rc != 0 {
		return nil, fmt.Errorf("libusb init: %s", usbErrName(rc))
	}

	var lastErr error
	for _, id := range ids {
		handle := C.libusb_open_device_with_vid_pid(ctx, C.uint16_t(id[0]), C.uint16_t(id[1]))
		if handle == nil {
			if lastErr == nil {
				lastErr = fmt.Errorf("QMI USB device %04x:%04x not found", id[0], id[1])
			}
			continue
		}
		tr, err := probeLibusbQMITransport(ctx, handle)
		if err == nil {
			return tr, nil
		}
		lastErr = fmt.Errorf("device %04x:%04x: %w", id[0], id[1], err)
		C.libusb_close(handle)
	}
	C.libusb_exit(ctx)
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("no QMI USB device found")
}

// parseLibusbPath converts a virtual QMI device path into USB ID pairs.
// Accepted forms: "2ca3:4006", "0x2ca3:0x4006", "2c7c:0125".
// An empty path or "auto" uses the default probe list.
func parseLibusbPath(path string) ([][2]uint16, error) {
	if path == "" || strings.TrimSpace(path) == "auto" {
		return qmiDeviceIDs, nil
	}
	parts := strings.Split(strings.TrimSpace(path), ":")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid QMI libusb path %q (want VID:PID, e.g. 2ca3:4006)", path)
	}
	v, err := parseHexUint16(parts[0])
	if err != nil {
		return nil, fmt.Errorf("invalid VID in path %q: %w", path, err)
	}
	p, err := parseHexUint16(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid PID in path %q: %w", path, err)
	}
	return [][2]uint16{{v, p}}, nil
}

func parseHexUint16(s string) (uint16, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	if s == "" {
		return 0, errors.New("empty hex value")
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, fmt.Errorf("invalid hex digit %q", c)
		}
		v = v*16 + uint64(d)
	}
	if v > 0xffff {
		return 0, errors.New("value out of uint16 range")
	}
	return uint16(v), nil
}

// probeLibusbQMITransport scans all class-0xFF bulk interfaces and returns the
// first one that responds to a QMI CTL_SYNC request.
func probeLibusbQMITransport(ctx *C.libusb_context, handle *C.libusb_device_handle) (*libusbQMITransport, error) {
	dev := C.libusb_get_device(handle)
	if dev == nil {
		return nil, errors.New("libusb device handle has no device")
	}
	var deviceDescriptor C.struct_libusb_device_descriptor
	if rc := C.libusb_get_device_descriptor(dev, &deviceDescriptor); rc != 0 {
		return nil, fmt.Errorf("get USB device descriptor: %s", usbErrName(rc))
	}
	vendorID := uint16(deviceDescriptor.idVendor)
	productID := uint16(deviceDescriptor.idProduct)
	var config *C.struct_libusb_config_descriptor
	if rc := C.libusb_get_active_config_descriptor(dev, &config); rc != 0 {
		return nil, fmt.Errorf("get active USB config descriptor: %s", usbErrName(rc))
	}
	defer C.libusb_free_config_descriptor(config)

	var candidates []usbQMICandidate
	interfaces := unsafe.Slice(config._interface, int(config.bNumInterfaces))
	for _, intf := range interfaces {
		altsettings := unsafe.Slice(intf.altsetting, int(intf.num_altsetting))
		for _, alt := range altsettings {
			class := byte(alt.bInterfaceClass)
			subclass := byte(alt.bInterfaceSubClass)
			protocol := byte(alt.bInterfaceProtocol)
			if class != 0xff {
				continue
			}
			// The DJI USB composition exposes an ADB function as vendor-specific
			// class ff/42/01. It has bulk IN/OUT endpoints but is not a QMI channel;
			// writing CTL_SYNC there can corrupt an unrelated ADB stream. Do not
			// treat endpoint shape alone as evidence that an interface speaks QMI.
			if subclass == 0x42 && protocol == 0x01 {
				continue
			}
			var endpointIn, endpointOut byte
			hasInterrupt := false
			endpoints := unsafe.Slice(alt.endpoint, int(alt.bNumEndpoints))
			for _, ep := range endpoints {
				attrs := byte(ep.bmAttributes) & byte(C.LIBUSB_TRANSFER_TYPE_MASK)
				if attrs == byte(C.LIBUSB_TRANSFER_TYPE_INTERRUPT) {
					hasInterrupt = true
					continue
				}
				if attrs != byte(C.LIBUSB_TRANSFER_TYPE_BULK) {
					continue
				}
				addr := byte(ep.bEndpointAddress)
				if addr&byte(C.LIBUSB_ENDPOINT_IN) != 0 {
					endpointIn = addr
				} else {
					endpointOut = addr
				}
			}
			if qmiInterfaceCandidate(vendorID, productID, class, subclass, protocol,
				hasInterrupt, endpointIn != 0, endpointOut != 0) {
				candidates = append(candidates, usbQMICandidate{
					iface:       int(alt.bInterfaceNumber),
					class:       class,
					subclass:    subclass,
					protocol:    protocol,
					endpointIn:  endpointIn,
					endpointOut: endpointOut,
				})
			}
		}
	}

	var probeErrors []string
	for _, candidate := range candidates {
		if rc := C.libusb_claim_interface(handle, C.int(candidate.iface)); rc != 0 {
			probeErrors = append(probeErrors, fmt.Sprintf("claim interface %d (%02x/%02x/%02x): %s",
				candidate.iface, candidate.class, candidate.subclass, candidate.protocol, usbErrName(rc)))
			continue
		}
		tr := &libusbQMITransport{
			ctx:         ctx,
			handle:      handle,
			iface:       candidate.iface,
			endpointIn:  candidate.endpointIn,
			endpointOut: candidate.endpointOut,
		}
		if err := tr.probeSync(); err == nil {
			return tr, nil
		} else {
			probeErrors = append(probeErrors, fmt.Sprintf("probe interface %d (%02x/%02x/%02x, out 0x%02x in 0x%02x): %v",
				candidate.iface, candidate.class, candidate.subclass, candidate.protocol,
				candidate.endpointOut, candidate.endpointIn, err))
		}
		C.libusb_release_interface(handle, C.int(candidate.iface))
	}
	if len(probeErrors) != 0 {
		return nil, fmt.Errorf("QMI USB interface probe failed: %s", strings.Join(probeErrors, "; "))
	}
	return nil, errors.New("no QMI bulk interface found")
}

// qmiInterfaceCandidate avoids transmitting QMI frames to unrelated vendor
// endpoints. DJI's QDC507 composition has diagnostic/NMEA interfaces before
// the AT/modem functions and an ADB interface after them. Its QMI-like
// interface must have the full ff/ff/ff signature plus interrupt and bulk
// endpoints; other DJI vendor interfaces are not safe QMI candidates.
func qmiInterfaceCandidate(vendorID, productID uint16, class, subclass, protocol byte,
	hasInterrupt, hasBulkIn, hasBulkOut bool) bool {
	if class != 0xff || (subclass == 0x42 && protocol == 0x01) ||
		!hasBulkIn || !hasBulkOut {
		return false
	}
	if vendorID == qmiLibusbVendorDJI && productID == qmiLibusbProductDJI {
		return subclass == 0xff && protocol == 0xff && hasInterrupt
	}
	return true
}

type usbQMICandidate struct {
	iface       int
	class       byte
	subclass    byte
	protocol    byte
	endpointIn  byte
	endpointOut byte
}

// probeSync sends a QMI CTL_SYNC request (service 0x00, message 0x0027) and
// waits for a valid CTL response frame. AT-only interfaces will not answer
// with a QMUX frame; a QMI channel echoes a valid control response.
func (t *libusbQMITransport) probeSync() error {
	req := buildCTLSyncFrame()
	if err := t.writeAll(req); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n, err := t.readOnce(buf, 500*time.Millisecond)
		if err != nil {
			if errors.Is(err, errLibusbTimeout) {
				continue
			}
			return err
		}
		if n < QmuxHeaderSize {
			continue
		}
		// QMUX header: byte0=0x01 IFType, bytes1-2 length (LE), byte3 flags,
		// byte4 service, byte5 client. CTL frames use service 0x00.
		if buf[0] == 0x01 && buf[4] == 0x00 {
			return nil
		}
	}
	return errors.New("QMI CTL_SYNC probe timed out (interface is not QMI)")
}

// buildCTLSyncFrame constructs a QMI CTL_SYNC request frame, byte-for-byte
// equivalent to what Packet.Marshal produces for
// SendRequest(ctx, ServiceControl, 0, CTLSync, nil):
//
//	QMUX header: 0x01 0x0B 0x00 | 0x00 0x00 0x00
//	  IFType=0x01, Length=0x000B (6 CTL bytes + 5 header tail), flags=0,
//	  ServiceType=0x00 (CTL), ClientID=0x00
//	CTL header : 0x00 0x01 0x27 0x00 0x00 0x00
//	  ControlFlags=0x00 (request), TransactionID=0x01,
//	  MessageID=0x0027 (CTL_SYNC), Length=0x0000 (no TLVs)
func buildCTLSyncFrame() []byte {
	return []byte{0x01, 0x0B, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x27, 0x00, 0x00, 0x00}
}

var errLibusbTimeout = errors.New("libusb bulk transfer timeout")

// Read implements qmiTransport.
func (tr *libusbQMITransport) Read(p []byte) (int, error) {
	tr.readMu.Lock()
	defer tr.readMu.Unlock()
	if tr.handle == nil {
		return 0, errors.New("QMI transport closed")
	}
	tr.readDeadlineMu.Lock()
	dl := tr.readDeadline
	tr.readDeadlineMu.Unlock()
	var timeout time.Duration
	if dl.IsZero() {
		timeout = 100 * time.Millisecond
	} else if d := time.Until(dl); d <= 0 {
		return 0, errLibusbTimeout
	} else {
		timeout = d
	}
	return tr.readOnce(p, timeout)
}

func (tr *libusbQMITransport) readOnce(p []byte, timeout time.Duration) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var transferred C.int
	rc := C.libusb_bulk_transfer(
		tr.handle,
		C.uchar(tr.endpointIn),
		(*C.uchar)(unsafe.Pointer(&p[0])),
		C.int(len(p)),
		&transferred,
		C.uint(timeout.Milliseconds()),
	)
	if rc == C.LIBUSB_ERROR_TIMEOUT {
		return 0, errLibusbTimeout
	}
	if rc != 0 {
		return 0, fmt.Errorf("USB bulk read: %s", usbErrName(rc))
	}
	return int(transferred), nil
}

// Write implements qmiTransport. A QMI request frame is sent in one bulk
// transfer; libusb either sends everything or fails.
func (tr *libusbQMITransport) Write(p []byte) (int, error) {
	tr.writeMu.Lock()
	defer tr.writeMu.Unlock()
	if tr.handle == nil {
		return 0, errors.New("QMI transport closed")
	}
	if len(p) == 0 {
		return 0, nil
	}
	var transferred C.int
	rc := C.libusb_bulk_transfer(
		tr.handle,
		C.uchar(tr.endpointOut),
		(*C.uchar)(unsafe.Pointer(&p[0])),
		C.int(len(p)),
		&transferred,
		C.uint(3000), // 3s write timeout
	)
	if rc != 0 {
		return 0, fmt.Errorf("USB bulk write: %s", usbErrName(rc))
	}
	return int(transferred), nil
}

func (tr *libusbQMITransport) writeAll(p []byte) error {
	for len(p) > 0 {
		n, err := tr.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("QMI USB write made no progress")
		}
		p = p[n:]
	}
	return nil
}

// SetReadDeadline implements qmiTransport.
func (tr *libusbQMITransport) SetReadDeadline(t time.Time) error {
	tr.readDeadlineMu.Lock()
	tr.readDeadline = t
	tr.readDeadlineMu.Unlock()
	return nil
}

// Close implements qmiTransport.
func (tr *libusbQMITransport) Close() error {
	tr.closeMu.Lock()
	defer tr.closeMu.Unlock()
	if tr.closed {
		return nil
	}
	tr.closed = true
	if tr.handle != nil {
		C.libusb_release_interface(tr.handle, C.int(tr.iface))
		C.libusb_close(tr.handle)
		tr.handle = nil
	}
	if tr.ctx != nil {
		C.libusb_exit(tr.ctx)
		tr.ctx = nil
	}
	return nil
}

// usbErrName mirrors the helper in cmd/proletarius-macos/usbat_darwin.go.
func usbErrName(rc C.int) string {
	return C.GoString(C.libusb_error_name(rc))
}

// ensure libusbQMITransport satisfies qmiTransport at compile time.
var _ qmiTransport = (*libusbQMITransport)(nil)
