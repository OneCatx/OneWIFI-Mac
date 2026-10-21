package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"
)

type nativeVoWiFiSessionPolicyResult struct {
	Operation    string `json:"operation"`
	DelaySeconds int64  `json:"delay_seconds"`
}

func nativeVoWiFiSessionDelay(ctx context.Context, operation string, valueKey string, value int) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	socket := os.Getenv("PROLETARIUS_CARRIER_SOCKET")
	if socket == "" {
		return 0, fmt.Errorf("Rust VoWiFi 会话策略服务未启动")
	}
	payloadValue := map[string]any{"operation": operation, valueKey: value}
	payload, err := json.Marshal(payloadValue)
	if err != nil {
		return 0, err
	}
	var result nativeVoWiFiSessionPolicyResult
	if err := requestNativeJSON(ctx, socket, "/api/v2/vowifi/session-policy", payload, &result); err != nil {
		return 0, err
	}
	if result.Operation != operation || result.DelaySeconds <= 0 {
		return 0, fmt.Errorf("Rust VoWiFi 会话策略返回无效")
	}
	return time.Duration(result.DelaySeconds) * time.Second, nil
}

func runtimeRegistrationRenewalDelay(ctx context.Context, expiresSeconds int) (time.Duration, error) {
	delay, err := nativeVoWiFiSessionDelay(ctx, "registration_renewal", "expires_seconds", expiresSeconds)
	if err != nil {
		return 0, fmt.Errorf("Rust VoWiFi 注册续期策略: %w", err)
	}
	log.Printf("[Reader VoWiFi] lifecycle policy_source=rust operation=registration_renewal delay=%s", delay)
	return delay, nil
}

func runtimeReconnectDelay(attempt int) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	delay, err := nativeVoWiFiSessionDelay(ctx, "reconnect", "attempt", attempt)
	if err != nil {
		return 0, fmt.Errorf("Rust VoWiFi 重连策略: %w", err)
	}
	log.Printf("[Reader VoWiFi] lifecycle policy_source=rust operation=reconnect attempt=%d delay=%s", attempt, delay)
	return delay, nil
}
