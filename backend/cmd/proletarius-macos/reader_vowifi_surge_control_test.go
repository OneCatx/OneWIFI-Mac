package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func withSurgeControlHooks(
	t *testing.T,
	available func() bool,
	status func(context.Context, string) (bool, error),
	enable func(context.Context) ([]byte, error),
) {
	t.Helper()
	previousAvailable := surgeCLIAvailable
	previousStatus := surgeFeatureStatus
	previousEnable := surgeEnableMode
	surgeCLIAvailable = available
	surgeFeatureStatus = status
	surgeEnableMode = enable
	t.Cleanup(func() {
		surgeCLIAvailable = previousAvailable
		surgeFeatureStatus = previousStatus
		surgeEnableMode = previousEnable
	})
}

func TestEnsureSurgeUDPModeNeverTouchesExplicitOrMacOSSocks(t *testing.T) {
	called := 0
	withSurgeControlHooks(
		t,
		func() bool { called++; return true },
		func(context.Context, string) (bool, error) { called++; return true, nil },
		func(context.Context) ([]byte, error) { called++; return nil, nil },
	)

	// This models Clash at 127.0.0.1:7897 while Surge is installed or retains
	// a feature state. The Rust source must remain explicit, so Go cannot
	// inspect or mutate Surge based on a matching local port.
	for _, source := range []string{"", "explicit", "macos-socks"} {
		if err := ensureSurgeUDPMode(context.Background(), source, source == ""); err == nil {
			t.Fatalf("source %q unexpectedly authorized Surge control", source)
		}
	}
	if called != 0 {
		t.Fatalf("non-Surge source unexpectedly inspected or changed Surge (%d calls)", called)
	}
}

func TestEnsureSurgeUDPModeUsesOnlyRustVerifiedSurgeSource(t *testing.T) {
	availableCalls := 0
	statusCalls := make([]string, 0, 2)
	enableCalls := 0
	withSurgeControlHooks(
		t,
		func() bool { availableCalls++; return true },
		func(_ context.Context, feature string) (bool, error) {
			statusCalls = append(statusCalls, feature)
			switch feature {
			case "system-proxy":
				return true, nil
			case "enhanced-mode":
				return false, nil
			default:
				return false, errors.New("unexpected feature")
			}
		},
		func(context.Context) ([]byte, error) {
			enableCalls++
			return []byte("fault-injected"), errors.New("injected failure")
		},
	)

	err := ensureSurgeUDPMode(context.Background(), "surge-effective-profile", true)
	if err == nil || !strings.Contains(err.Error(), "自动开启增强模式失败") {
		t.Fatalf("expected injected Surge enable failure, got %v", err)
	}
	if availableCalls != 1 || enableCalls != 1 {
		t.Fatalf("available=%d enable=%d, want one controlled Surge attempt", availableCalls, enableCalls)
	}
	if got := strings.Join(statusCalls, ","); got != "system-proxy,enhanced-mode" {
		t.Fatalf("feature checks = %q", got)
	}
}

func TestEnsureSurgeUDPModeFailsClosedWhenControlPlaneCannotConfirmOwnership(t *testing.T) {
	tests := []struct {
		name      string
		available func() bool
		status    func(context.Context, string) (bool, error)
		contains  string
	}{
		{
			name:      "cli unavailable",
			available: func() bool { return false },
			status:    func(context.Context, string) (bool, error) { return true, nil },
			contains:  "surge-cli 不可用",
		},
		{
			name:      "system proxy off",
			available: func() bool { return true },
			status: func(_ context.Context, feature string) (bool, error) {
				if feature != "system-proxy" {
					return false, errors.New("unexpected feature")
				}
				return false, nil
			},
			contains: "系统代理当前未开启",
		},
		{
			name:      "system proxy unknown",
			available: func() bool { return true },
			status: func(_ context.Context, feature string) (bool, error) {
				if feature != "system-proxy" {
					return false, errors.New("unexpected feature")
				}
				return false, errors.New("status unavailable")
			},
			contains: "无法确认 Surge 当前系统代理状态",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			enableCalls := 0
			withSurgeControlHooks(
				t,
				test.available,
				test.status,
				func(context.Context) ([]byte, error) {
					enableCalls++
					return nil, nil
				},
			)

			err := ensureSurgeUDPMode(context.Background(), "surge-effective-profile", true)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error=%v, want actionable %q", err, test.contains)
			}
			if enableCalls != 0 {
				t.Fatalf("enable calls=%d, want no mutation without ownership confirmation", enableCalls)
			}
		})
	}
}

func TestShouldManageSurgeEnhancedModeRequiresVerifiedSource(t *testing.T) {
	if !shouldManageSurgeEnhancedMode("surge-effective-profile") {
		t.Fatal("verified Surge source was rejected")
	}
	for _, source := range []string{"explicit", "macos-socks", "  explicit  ", ""} {
		if shouldManageSurgeEnhancedMode(source) {
			t.Fatalf("unverified source %q was accepted", source)
		}
	}
}

func TestSelectReaderTransportForStartupRecoversOnlyVerifiedSurgeRejection(t *testing.T) {
	first := nativeReaderTransportSelection{
		Mode:        "rejected",
		ProxySource: "surge-effective-profile",
		ErrorCode:   "socks5_udp_unavailable",
		ProxyError:  "socks5_handshake_failed",
	}
	second := nativeReaderTransportSelection{
		Mode:        "enhanced-mode-fake-ip",
		Address:     "198.18.0.14",
		ProxySource: "surge-effective-profile",
	}
	selectionCalls := 0
	previousSelect := readerSelectTransport
	readerSelectTransport = func(context.Context, string, string) (nativeReaderTransportSelection, error) {
		selectionCalls++
		if selectionCalls == 1 {
			return first, &nativeTransportSelectionError{selection: first, cause: errors.New("first selection rejected")}
		}
		return second, nil
	}
	t.Cleanup(func() { readerSelectTransport = previousSelect })

	enabled := false
	enableCalls := 0
	withSurgeControlHooks(
		t,
		func() bool { return true },
		func(_ context.Context, feature string) (bool, error) {
			switch feature {
			case "system-proxy":
				return true, nil
			case "enhanced-mode":
				return enabled, nil
			default:
				return false, errors.New("unexpected feature")
			}
		},
		func(context.Context) ([]byte, error) {
			enableCalls++
			enabled = true
			return nil, nil
		},
	)

	selection, err := selectReaderTransportForStartup(context.Background(), "", "epdg.example", true)
	if err != nil {
		t.Fatalf("recovered selection failed: %v", err)
	}
	if selection.factory == nil || selection.mode != "enhanced-mode-fake-ip" {
		t.Fatalf("unexpected recovered selection: %+v", selection)
	}
	if selectionCalls != 2 || enableCalls != 1 {
		t.Fatalf("selection calls=%d enable calls=%d, want exactly one recovery retry", selectionCalls, enableCalls)
	}
}

func TestSelectReaderTransportForStartupRecoversRustVerifiedSurgeDirectRoute(t *testing.T) {
	first := nativeReaderTransportSelection{
		Mode:        "direct",
		Address:     "87.194.8.8",
		ProxySource: "surge-effective-profile",
	}
	second := nativeReaderTransportSelection{
		Mode:        "enhanced-mode-fake-ip",
		Address:     "198.18.0.14",
		ProxySource: "surge-effective-profile",
	}
	selectionCalls := 0
	previousSelect := readerSelectTransport
	readerSelectTransport = func(context.Context, string, string) (nativeReaderTransportSelection, error) {
		selectionCalls++
		if selectionCalls == 1 {
			return first, nil
		}
		return second, nil
	}
	t.Cleanup(func() { readerSelectTransport = previousSelect })

	enhanced := false
	enableCalls := 0
	withSurgeControlHooks(
		t,
		func() bool { return true },
		func(_ context.Context, feature string) (bool, error) {
			switch feature {
			case "system-proxy":
				return true, nil
			case "enhanced-mode":
				return enhanced, nil
			default:
				return false, errors.New("unexpected feature")
			}
		},
		func(context.Context) ([]byte, error) {
			enableCalls++
			enhanced = true
			return nil, nil
		},
	)

	selection, err := selectReaderTransportForStartup(context.Background(), "", "epdg.example", true)
	if err != nil {
		t.Fatalf("direct-route recovery failed: %v", err)
	}
	if selection.mode != "enhanced-mode-fake-ip" || selection.factory == nil {
		t.Fatalf("selection=%+v, want verified enhanced-mode route", selection)
	}
	if selectionCalls != 2 || enableCalls != 1 {
		t.Fatalf("selection calls=%d enable calls=%d, want initial + one post-enable reselect", selectionCalls, enableCalls)
	}
}

func TestSelectReaderTransportForStartupFailsIfPostEnableRustOwnershipChanges(t *testing.T) {
	first := nativeReaderTransportSelection{
		Mode:        "rejected",
		ProxySource: "surge-effective-profile",
		ErrorCode:   "socks5_udp_unavailable",
	}
	second := nativeReaderTransportSelection{
		Mode:        "enhanced-mode-fake-ip",
		Address:     "198.18.0.14",
		ProxySource: "macos-socks",
	}
	selectionCalls := 0
	previousSelect := readerSelectTransport
	readerSelectTransport = func(context.Context, string, string) (nativeReaderTransportSelection, error) {
		selectionCalls++
		if selectionCalls == 1 {
			return first, &nativeTransportSelectionError{selection: first, cause: errors.New("rejected")}
		}
		return second, nil
	}
	t.Cleanup(func() { readerSelectTransport = previousSelect })

	enabled := false
	enableCalls := 0
	withSurgeControlHooks(
		t,
		func() bool { return true },
		func(_ context.Context, feature string) (bool, error) {
			switch feature {
			case "system-proxy":
				return true, nil
			case "enhanced-mode":
				return enabled, nil
			default:
				return false, errors.New("unexpected feature")
			}
		},
		func(context.Context) ([]byte, error) {
			enableCalls++
			enabled = true
			return nil, nil
		},
	)

	_, err := selectReaderTransportForStartup(context.Background(), "", "epdg.example", true)
	if err == nil || !strings.Contains(err.Error(), "未确认当前代理仍由 Surge 接管") {
		t.Fatalf("ownership-change error=%v", err)
	}
	if selectionCalls != 2 || enableCalls != 1 {
		t.Fatalf("selection calls=%d enable calls=%d, want one bounded recovery", selectionCalls, enableCalls)
	}
}

func TestSelectReaderTransportForStartupLeavesWorkingSurgeRouteUntouched(t *testing.T) {
	selection := nativeReaderTransportSelection{
		Mode:          "socks5-udp-domain",
		ProxyEndpoint: "127.0.0.1:6153",
		ProxySource:   "surge-effective-profile",
	}
	previousSelect := readerSelectTransport
	readerSelectTransport = func(context.Context, string, string) (nativeReaderTransportSelection, error) {
		return selection, nil
	}
	t.Cleanup(func() { readerSelectTransport = previousSelect })
	surgeCalls := 0
	withSurgeControlHooks(
		t,
		func() bool { surgeCalls++; return true },
		func(context.Context, string) (bool, error) { surgeCalls++; return true, nil },
		func(context.Context) ([]byte, error) { surgeCalls++; return nil, nil },
	)

	result, err := selectReaderTransportForStartup(context.Background(), "", "epdg.example", true)
	if err != nil || result.factory == nil || result.mode != "socks5-udp-domain" {
		t.Fatalf("working route changed: result=%+v err=%v", result, err)
	}
	if surgeCalls != 0 {
		t.Fatalf("working Rust-selected Surge route unexpectedly changed Enhanced Mode (%d calls)", surgeCalls)
	}
}

func TestSelectReaderTransportForStartupNeverRecoversExplicitOrUnrelatedFailure(t *testing.T) {
	tests := []struct {
		name      string
		automatic bool
		selection nativeReaderTransportSelection
	}{
		{
			name:      "explicit clash endpoint",
			automatic: false,
			selection: nativeReaderTransportSelection{Mode: "rejected", ProxySource: "explicit", ErrorCode: "socks5_udp_unavailable"},
		},
		{
			name:      "macos socks endpoint",
			automatic: true,
			selection: nativeReaderTransportSelection{Mode: "rejected", ProxySource: "macos-socks", ErrorCode: "socks5_udp_unavailable"},
		},
		{
			name:      "verified surge non recoverable error",
			automatic: true,
			selection: nativeReaderTransportSelection{Mode: "rejected", ProxySource: "surge-effective-profile", ErrorCode: "transport_selection_timeout"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selectionCalls := 0
			previousSelect := readerSelectTransport
			readerSelectTransport = func(context.Context, string, string) (nativeReaderTransportSelection, error) {
				selectionCalls++
				return test.selection, &nativeTransportSelectionError{selection: test.selection, cause: errors.New("rejected")}
			}
			t.Cleanup(func() { readerSelectTransport = previousSelect })
			surgeCalls := 0
			withSurgeControlHooks(
				t,
				func() bool { surgeCalls++; return true },
				func(context.Context, string) (bool, error) { surgeCalls++; return true, nil },
				func(context.Context) ([]byte, error) { surgeCalls++; return nil, nil },
			)

			if _, err := selectReaderTransportForStartup(context.Background(), "127.0.0.1:7897", "epdg.example", test.automatic); err == nil {
				t.Fatal("rejected selection unexpectedly succeeded")
			}
			if selectionCalls != 1 || surgeCalls != 0 {
				t.Fatalf("selection calls=%d surge calls=%d, recovery was not safely gated", selectionCalls, surgeCalls)
			}
		})
	}
}
