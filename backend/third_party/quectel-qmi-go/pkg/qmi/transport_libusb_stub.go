//go:build darwin && !cgo

package qmi

import (
	"fmt"
)

// openLibusbRawTransport is only defined when cgo is enabled; this stub keeps
// the darwin build consistent and documents the requirement.
func openLibusbRawTransport(path string) (qmiTransport, error) {
	return nil, fmt.Errorf("QMI over libusb requires cgo on macOS (rebuild with CGO_ENABLED=1 and libusb installed)")
}