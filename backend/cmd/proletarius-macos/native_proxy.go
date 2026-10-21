package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Rust bounds the entire automatic ePDG transport choice below seven seconds
// (while retaining a six-second explicit SOCKS UDP probe). Give the local
// control request a small IPC/JSON margin without changing unrelated native
// endpoint timeouts.
const nativeTransportSelectionTimeout = 8 * time.Second

type nativeProxyProbeResult struct {
	OK        bool   `json:"ok"`
	Latency   int64  `json:"latency_ms"`
	UDPReady  bool   `json:"udp_ready"`
	ErrorCode string `json:"error_code"`
}

type nativeReaderTransportSelection struct {
	Mode      string `json:"mode"`
	Address   string `json:"address"`
	Interface string `json:"interface"`
	// ProxyEndpoint is supplied only for socks5-udp-domain after the Rust
	// control plane has resolved and completed a UDP ASSOCIATE handshake with
	// it. It prevents a second system-proxy/DNS discovery between policy
	// selection and Go's live transport startup.
	ProxyEndpoint string `json:"proxy_endpoint"`
	ProxySource   string `json:"proxy_source"`
	ErrorCode     string `json:"error_code"`
	ProxyError    string `json:"proxy_error"`
}

// nativeTransportSelectionError preserves Rust's structured rejection while it
// crosses the small Go compatibility boundary. Callers must not infer a
// proxy owner from a port number after an error: proxySource remains the only
// authority for the narrowly scoped Surge recovery path.
type nativeTransportSelectionError struct {
	selection nativeReaderTransportSelection
	cause     error
}

func (e *nativeTransportSelectionError) Error() string {
	if e == nil || e.cause == nil {
		return "Rust ePDG 传输不可用"
	}
	return e.cause.Error()
}

func (e *nativeTransportSelectionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func selectRuntimeReaderTransport(ctx context.Context, proxy, epdg string) (nativeReaderTransportSelection, error) {
	if err := ctx.Err(); err != nil {
		return nativeReaderTransportSelection{}, err
	}
	socket := os.Getenv("PROLETARIUS_CARRIER_SOCKET")
	if socket == "" {
		return nativeReaderTransportSelection{}, fmt.Errorf("Rust ePDG 传输选择服务未启动")
	}
	payload, err := json.Marshal(map[string]string{
		"proxy": strings.TrimSpace(proxy),
		"epdg":  strings.TrimSpace(epdg),
	})
	if err != nil {
		return nativeReaderTransportSelection{}, err
	}
	var result nativeReaderTransportSelection
	if err := requestNativeJSONWithTimeout(ctx, socket, "/api/v2/vowifi/select-transport", payload, &result, nativeTransportSelectionTimeout); err != nil {
		return nativeReaderTransportSelection{}, fmt.Errorf("Rust ePDG 传输选择失败: %w", err)
	}
	if result.Mode == "" || result.Mode == "rejected" {
		code := result.ErrorCode
		if code == "" {
			code = "transport_unavailable"
		}
		message := fmt.Errorf("Rust ePDG 传输不可用: %s", code)
		if diagnostic := strings.TrimSpace(result.ProxyError); diagnostic != "" {
			message = fmt.Errorf("%w（系统代理诊断：%s）", message, diagnostic)
		}
		return result, &nativeTransportSelectionError{selection: result, cause: message}
	}
	return result, nil
}

func probeRuntimeProxy(ctx context.Context, proxy string) (nativeProxyProbeResult, bool, error) {
	if err := ctx.Err(); err != nil {
		return nativeProxyProbeResult{}, false, err
	}
	socket := os.Getenv("PROLETARIUS_CARRIER_SOCKET")
	if socket == "" {
		return nativeProxyProbeResult{}, false, nil
	}
	payload, err := json.Marshal(map[string]string{"proxy": strings.TrimSpace(proxy)})
	if err != nil {
		return nativeProxyProbeResult{}, true, err
	}
	var result nativeProxyProbeResult
	if err := requestNativeJSON(ctx, socket, "/api/v2/vowifi/proxy-probe", payload, &result); err != nil {
		return nativeProxyProbeResult{}, true, err
	}
	if !result.OK {
		if result.ErrorCode == "" {
			result.ErrorCode = "socks5_handshake_failed"
		}
		return result, true, fmt.Errorf("Rust SOCKS5 检测失败: %s", result.ErrorCode)
	}
	return result, true, nil
}
