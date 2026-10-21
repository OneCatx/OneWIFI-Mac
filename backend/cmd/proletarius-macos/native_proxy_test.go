package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProbeRuntimeProxyUsesNativeCapabilityResult(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pc-proxy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "proxy.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/vowifi/proxy-probe" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input["proxy"] != "127.0.0.1:7897" {
			t.Fatalf("unexpected input: %#v (%v)", input, err)
		}
		writeJSON(w, http.StatusOK, nativeProxyProbeResult{OK: true, Latency: 2, UDPReady: true})
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	t.Setenv("PROLETARIUS_APP_TOKEN", "fixture-token")
	result, native, err := probeRuntimeProxy(context.Background(), " 127.0.0.1:7897 ")
	if err != nil || !native || !result.OK || !result.UDPReady || result.Latency != 2 {
		t.Fatalf("result=%+v native=%v err=%v", result, native, err)
	}
}

func TestProbeRuntimeProxyAllowsLegacyFallbackWithoutRustSocket(t *testing.T) {
	previous, present := os.LookupEnv("PROLETARIUS_CARRIER_SOCKET")
	_ = os.Unsetenv("PROLETARIUS_CARRIER_SOCKET")
	defer func() {
		if present {
			_ = os.Setenv("PROLETARIUS_CARRIER_SOCKET", previous)
		}
	}()
	_, native, err := probeRuntimeProxy(context.Background(), "127.0.0.1:7897")
	if err != nil || native {
		t.Fatalf("native=%v err=%v", native, err)
	}
}

func TestSelectRuntimeReaderTransportUsesRustDecision(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pc-transport-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "transport.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/vowifi/select-transport" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input["proxy"] != "127.0.0.1:7897" || input["epdg"] != "epdg.example" {
			t.Fatalf("unexpected input: %#v (%v)", input, err)
		}
		writeJSON(w, http.StatusOK, nativeReaderTransportSelection{
			Mode:          "socks5-udp-domain",
			ProxyEndpoint: "127.0.0.1:7897",
			ProxySource:   "explicit",
		})
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	t.Setenv("PROLETARIUS_APP_TOKEN", "fixture-token")
	result, err := selectRuntimeReaderTransport(context.Background(), " 127.0.0.1:7897 ", " epdg.example ")
	if err != nil || result.Mode != "socks5-udp-domain" || result.ProxyEndpoint != "127.0.0.1:7897" || result.ProxySource != "explicit" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestSelectRuntimeReaderTransportPreservesStructuredRustRejection(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pc-transport-rejected-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "transport.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, nativeReaderTransportSelection{
			Mode:        "rejected",
			ProxySource: "surge-effective-profile",
			ErrorCode:   "unverified_public_route",
			ProxyError:  "socks5_handshake_failed",
		})
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	t.Setenv("PROLETARIUS_APP_TOKEN", "fixture-token")

	selection, err := selectRuntimeReaderTransport(context.Background(), "", "epdg.example")
	if err == nil {
		t.Fatal("rejected Rust selection unexpectedly succeeded")
	}
	var rejected *nativeTransportSelectionError
	if !errors.As(err, &rejected) {
		t.Fatalf("error did not preserve structured rejection: %T %v", err, err)
	}
	if selection != rejected.selection || rejected.selection.ProxySource != "surge-effective-profile" || rejected.selection.ErrorCode != "unverified_public_route" {
		t.Fatalf("structured selection lost: selection=%+v error=%+v", selection, rejected.selection)
	}
}

func TestSelectRuntimeReaderTransportHonorsStartupDeadline(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pc-transport-deadline-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "transport.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		// The client context, rather than a background startup request, must
		// stop this deliberately stalled local control response.
		<-request.Context().Done()
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	t.Setenv("PROLETARIUS_APP_TOKEN", "fixture-token")

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = selectRuntimeReaderTransport(ctx, "127.0.0.1:7897", "epdg.example")
	if err == nil {
		t.Fatal("stalled native selection unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("selection ignored caller deadline: %v", elapsed)
	}
}

func TestAutomaticRustSocksSelectionFeedsGoTransportFactory(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pc-transport-auto-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "transport.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/vowifi/select-transport" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input["proxy"] != "" || input["epdg"] != "epdg.example" {
			t.Fatalf("unexpected input: %#v (%v)", input, err)
		}
		writeJSON(w, http.StatusOK, nativeReaderTransportSelection{
			Mode:          "socks5-udp-domain",
			ProxyEndpoint: "127.0.0.1:7897",
			ProxySource:   "surge-effective-profile",
		})
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	t.Setenv("PROLETARIUS_APP_TOKEN", "fixture-token")

	factory, mode, err := selectReaderSWUTransport(context.Background(), "", "epdg.example")
	if err != nil || factory == nil || mode != "socks5-udp-domain" {
		t.Fatalf("factory:%t mode:%q err:%v", factory != nil, mode, err)
	}
}
