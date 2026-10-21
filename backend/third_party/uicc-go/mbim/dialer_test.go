package mbim

import (
	"context"
	"testing"
)

type stubDialer struct{}

func (stubDialer) Dial(context.Context) (Conn, error) {
	return nil, nil
}

func TestDialerUsesProxy(t *testing.T) {
	tests := []struct {
		name string
		in   Dialer
		want bool
	}{
		{"proxy", ProxyDialer{}, true},
		{"custom", stubDialer{}, false},
		{"nil", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dialerUsesProxy(tt.in); got != tt.want {
				t.Fatalf("dialerUsesProxy() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpenOptionsSetDialer(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want Dialer
	}{
		{"proxy", []Option{WithProxy("/dev/cdc-wdm0")}, ProxyDialer{Device: "/dev/cdc-wdm0"}},
		{"direct", []Option{WithDirect("/dev/cdc-wdm0")}, DirectDialer{Device: "/dev/cdc-wdm0"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config{}
			for _, opt := range tt.opts {
				opt(&cfg)
			}
			if cfg.dialer != tt.want {
				t.Fatalf("dialer = %#v, want %#v", cfg.dialer, tt.want)
			}
		})
	}
}

func TestOpenOptionsSetSlot(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want int
	}{
		{"default", nil, 1},
		{"custom", []Option{WithSlot(2)}, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config{slot: 1}
			for _, opt := range tt.opts {
				opt(&cfg)
			}
			if cfg.slot != tt.want {
				t.Fatalf("slot = %d, want %d", cfg.slot, tt.want)
			}
		})
	}
}
