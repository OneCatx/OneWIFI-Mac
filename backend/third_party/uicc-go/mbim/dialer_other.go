//go:build !linux

package mbim

import (
	"context"
	"errors"
)

type DirectDialer struct {
	Device string
}

func (d ProxyDialer) Dial(context.Context) (Conn, error) {
	return nil, errors.New("opening MBIM reader: mbim-proxy is only supported on linux")
}

func (d DirectDialer) Dial(context.Context) (Conn, error) {
	return nil, errors.New("opening MBIM device: direct access is only supported on linux")
}

func (d DirectDialer) device() string { return d.Device }
