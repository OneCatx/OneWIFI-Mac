//go:build linux

package mbim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/damonto/uicc-go/cdcwdm"
)

type DirectDialer struct {
	Device string
}

func (d ProxyDialer) Dial(ctx context.Context) (Conn, error) {
	address := d.Address
	if address == "" {
		address = "\x00mbim-proxy"
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", address)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (d DirectDialer) Dial(ctx context.Context) (Conn, error) {
	device := strings.TrimSpace(d.Device)
	if device == "" {
		return nil, errors.New("opening MBIM device: device is empty")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	conn, err := cdcwdm.Open(device)
	if err != nil {
		return nil, fmt.Errorf("opening MBIM device: %w", err)
	}
	return conn, nil
}

func (d DirectDialer) device() string { return strings.TrimSpace(d.Device) }
