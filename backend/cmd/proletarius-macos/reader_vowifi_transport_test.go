package main

import (
	"context"
	"errors"
	"net"
	"testing"
)

func withReaderTransportSelection(t *testing.T, selection nativeReaderTransportSelection, selectionErr error) {
	t.Helper()
	previous := readerSelectTransport
	readerSelectTransport = func(context.Context, string, string) (nativeReaderTransportSelection, error) {
		return selection, selectionErr
	}
	t.Cleanup(func() { readerSelectTransport = previous })
}

func TestSelectReaderSWUTransportConsumesRustSelection(t *testing.T) {
	tests := []struct {
		name      string
		proxy     string
		selection nativeReaderTransportSelection
		wantMode  string
	}{
		{"fake ip", "127.0.0.1:7897", nativeReaderTransportSelection{Mode: "enhanced-mode-fake-ip", Address: "198.18.0.14"}, "enhanced-mode-fake-ip"},
		{"domain socks", "127.0.0.1:7897", nativeReaderTransportSelection{Mode: "socks5-udp-domain", ProxyEndpoint: "127.0.0.1:7897", ProxySource: "explicit"}, "socks5-udp-domain"},
		{"utun", "127.0.0.1:6153", nativeReaderTransportSelection{Mode: "enhanced-mode-utun", Address: "87.194.8.8", Interface: "utun8"}, "enhanced-mode-utun"},
		{"direct", "", nativeReaderTransportSelection{Mode: "direct", Address: "87.194.8.8"}, "direct"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			withReaderTransportSelection(t, test.selection, nil)
			factory, mode, err := selectReaderSWUTransport(context.Background(), test.proxy, "epdg.example")
			if err != nil || factory == nil || mode != test.wantMode {
				t.Fatalf("selection = factory:%t mode:%q err:%v", factory != nil, mode, err)
			}
		})
	}
}

func TestReaderTransportKeepsRustSystemProxySourceAndDiagnostic(t *testing.T) {
	withReaderTransportSelection(t, nativeReaderTransportSelection{
		Mode:        "direct",
		Address:     "87.194.8.8",
		ProxySource: "",
		ProxyError:  "system_proxy_pac_unsupported",
	}, nil)
	selection, err := selectReaderSWUTransportWithSelection(context.Background(), "", "epdg.example")
	if err != nil || selection.factory == nil || selection.mode != "direct" {
		t.Fatalf("selection=%+v err=%v", selection, err)
	}
	if selection.proxySource != "" || selection.proxyError != "system_proxy_pac_unsupported" {
		t.Fatalf("Rust metadata was changed by Go: %+v", selection)
	}
}

func TestSelectReaderSWUTransportRejectsInvalidNativeResults(t *testing.T) {
	tests := []nativeReaderTransportSelection{
		{Mode: "enhanced-mode-fake-ip", Address: "not-an-ip"},
		{Mode: "enhanced-mode-utun", Address: "87.194.8.8", Interface: "en0"},
		{Mode: "unknown"},
	}
	for _, selection := range tests {
		t.Run(selection.Mode+selection.Interface, func(t *testing.T) {
			withReaderTransportSelection(t, selection, nil)
			if _, _, err := selectReaderSWUTransport(context.Background(), "127.0.0.1:7897", "epdg.example"); err == nil {
				t.Fatalf("invalid selection accepted: %+v", selection)
			}
		})
	}
}

func TestSelectReaderSWUTransportConsumesRustBoundSocksProxy(t *testing.T) {
	tests := []struct {
		name      string
		selection nativeReaderTransportSelection
		wantErr   bool
	}{
		{
			name:      "automatic discovery endpoint",
			selection: nativeReaderTransportSelection{Mode: "socks5-udp-domain", ProxyEndpoint: "127.0.0.1:7897", ProxySource: "surge-effective-profile"},
		},
		{
			name:      "missing endpoint",
			selection: nativeReaderTransportSelection{Mode: "socks5-udp-domain", ProxySource: "macos-socks"},
			wantErr:   true,
		},
		{
			name:      "malformed endpoint",
			selection: nativeReaderTransportSelection{Mode: "socks5-udp-domain", ProxyEndpoint: "not-an-endpoint", ProxySource: "macos-socks"},
			wantErr:   true,
		},
		{
			name:      "missing source",
			selection: nativeReaderTransportSelection{Mode: "socks5-udp-domain", ProxyEndpoint: "127.0.0.1:7897"},
			wantErr:   true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			withReaderTransportSelection(t, test.selection, nil)
			factory, mode, err := selectReaderSWUTransport(context.Background(), "", "epdg.example")
			if test.wantErr {
				if err == nil {
					t.Fatalf("selection accepted: %+v", test.selection)
				}
				return
			}
			if err != nil || factory == nil || mode != "socks5-udp-domain" {
				t.Fatalf("selection = factory:%t mode:%q err:%v", factory != nil, mode, err)
			}
		})
	}
}

func TestSelectReaderSWUTransportPropagatesNativeFailure(t *testing.T) {
	withReaderTransportSelection(t, nativeReaderTransportSelection{}, errors.New("native unavailable"))
	if _, _, err := selectReaderSWUTransport(context.Background(), "", "epdg.example"); err == nil {
		t.Fatal("native selection failure was ignored")
	}
}

func TestFakeIPTransportFactoryKeepsVerifiedAddress(t *testing.T) {
	factory := newReaderSWUTransparentTransportFactory(net.ParseIP("198.18.0.14"))
	transport, err := factory("", "epdg.example:500")
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	defer transport.Stop()
	remote, ok := transport.(interface{ RemoteAddrString() string })
	if !ok {
		t.Fatal("transport does not expose RemoteAddrString")
	}
	if got := remote.RemoteAddrString(); got != "198.18.0.14:500" {
		t.Fatalf("remote = %q", got)
	}
}

func TestIsReaderIKETimeout(t *testing.T) {
	if !isReaderIKETimeout(errors.New("timeout reached max retries")) {
		t.Fatal("English timeout was not recognized")
	}
	if !isReaderIKETimeout(errors.New("IKE 请求硬超时")) {
		t.Fatal("Chinese timeout was not recognized")
	}
	if isReaderIKETimeout(errors.New("authentication failed")) {
		t.Fatal("non-timeout error was misclassified")
	}
}
