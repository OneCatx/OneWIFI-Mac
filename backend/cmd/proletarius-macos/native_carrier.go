package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"proletarius.local/core/internal/vowifi/carrier"
)

func resolveRuntimeCarrier(sim carrier.SIMIdentity) (*carrier.Profile, error) {
	return resolveRuntimeCarrierContext(context.Background(), sim)
}

func resolveRuntimeCarrierContext(ctx context.Context, sim carrier.SIMIdentity) (*carrier.Profile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	socket := os.Getenv("PROLETARIUS_CARRIER_SOCKET")
	if socket == "" {
		return carrier.ResolveWithError(sim)
	}
	payload, err := json.Marshal(map[string]string{"mcc": sim.MCC, "mnc": sim.MNC, "imsi": sim.IMSI, "iccid": sim.ICCID, "gid1": sim.GID1, "gid2": sim.GID2, "spn": sim.SPN})
	if err != nil {
		return nil, err
	}
	var response struct {
		Revision       string                        `json:"revision"`
		RuntimeProfile *carrier.NativeRuntimeProfile `json:"runtime_profile"`
	}
	if err := requestNativeJSON(ctx, socket, "/api/v2/carrier/resolve", payload, &response); err != nil {
		return nil, err
	}
	if response.Revision == "" || response.RuntimeProfile == nil {
		return nil, fmt.Errorf("incomplete Rust carrier runtime profile")
	}
	profile := response.RuntimeProfile.Expanded()
	if profile.MCC != sim.MCC || profile.MNC != sim.MNC || profile.ID == "" {
		return nil, fmt.Errorf("Rust carrier profile identity mismatch")
	}
	return profile, nil
}

func requestNativeJSON(ctx context.Context, socket, path string, payload []byte, output any) error {
	return requestNativeJSONWithTimeout(ctx, socket, path, payload, output, 7*time.Second)
}

type nativeHTTPError struct {
	status int
	code   string
}

func (e *nativeHTTPError) Error() string {
	if e.code != "" {
		return fmt.Sprintf("Rust 本机服务拒绝请求（HTTP %d：%s）", e.status, e.code)
	}
	return fmt.Sprintf("Rust 本机服务拒绝请求（HTTP %d）", e.status)
}

func requestNativeJSONWithTimeout(ctx context.Context, socket, path string, payload []byte, output any, timeout time.Duration) error {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost"+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Proletarius-App-Token", os.Getenv("PROLETARIUS_APP_TOKEN"))
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("Rust 本机服务不可用: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil {
		return err
	}
	if len(raw) > 1024*1024 {
		return fmt.Errorf("native response too large")
	}
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(raw, &failure) == nil && safeNativeFailureCode(failure.Code) {
			return &nativeHTTPError{status: response.StatusCode, code: failure.Code}
		}
		return &nativeHTTPError{status: response.StatusCode}
	}
	if err := json.Unmarshal(raw, output); err != nil {
		return fmt.Errorf("invalid native response")
	}
	return nil
}

func safeNativeFailureCode(code string) bool {
	if code == "" || len(code) > 80 {
		return false
	}
	for _, character := range code {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}
