package mbim

import (
	"context"
	"io"
	"strings"
	"time"
)

type Conn interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

type maxControlTransferer interface {
	MaxControlTransfer() int
}

type Dialer interface {
	Dial(ctx context.Context) (Conn, error)
}

type ProxyDialer struct {
	Address string
	Device  string
}

type proxyDialer interface {
	usesProxy() bool
}

type deviceDialer interface {
	device() string
}

func dialerUsesProxy(d Dialer) bool {
	p, ok := d.(proxyDialer)
	return ok && p.usesProxy()
}

func (d ProxyDialer) usesProxy() bool { return true }

func (d ProxyDialer) device() string { return strings.TrimSpace(d.Device) }
