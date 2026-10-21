package main

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestValidateReaderVoWiFiProxy(t *testing.T) {
	tests := []struct {
		name    string
		address string
		wantErr bool
	}{
		{name: "local proxy", address: "127.0.0.1:1080"},
		{name: "lan proxy", address: "192.168.1.2:1080"},
		{name: "ipv6 proxy", address: "[::1]:1080"},
		{name: "missing proxy (direct mode)", wantErr: false},
		{name: "missing port", address: "127.0.0.1", wantErr: true},
		{name: "bad port", address: "127.0.0.1:70000", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateReaderVoWiFiProxy(test.address)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateReaderVoWiFiProxy(%q) error = %v, wantErr %v", test.address, err, test.wantErr)
			}
		})
	}
}

func TestProbeSOCKS5DoesNotNeedExternalNetwork(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		greeting := make([]byte, 3)
		if _, readErr := io.ReadFull(conn, greeting); readErr == nil {
			_, _ = conn.Write([]byte{0x05, 0x00})
		}
	}()
	if _, err := probeSOCKS5(listener.Addr().String(), time.Second); err != nil {
		t.Fatalf("probeSOCKS5: %v", err)
	}
	<-done
}

func TestExtractHTTPBody(t *testing.T) {
	got := extractHTTPBody([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}"))
	if string(got) != "{}" {
		t.Fatalf("body = %q", got)
	}
	if got := extractHTTPBody([]byte("invalid")); got != nil {
		t.Fatalf("invalid response body = %q", got)
	}
}
