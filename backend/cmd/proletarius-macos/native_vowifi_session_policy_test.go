package main

import (
	"context"
	"strings"
	"testing"
)

func TestRuntimeSessionPolicyFailsClosedWithoutRustSocket(t *testing.T) {
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", "")
	if _, err := runtimeRegistrationRenewalDelay(context.Background(), 2400); err == nil || !strings.Contains(err.Error(), "策略服务未启动") {
		t.Fatalf("renewal error=%v, want unavailable Rust policy", err)
	}
	if _, err := runtimeReconnectDelay(9); err == nil || !strings.Contains(err.Error(), "策略服务未启动") {
		t.Fatalf("reconnect error=%v, want unavailable Rust policy", err)
	}
}
