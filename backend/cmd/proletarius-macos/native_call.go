package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// resolveRuntimeDTMF moves keypad validation to the authenticated Rust
// gateway. It runs once per key press, never once per PCM or RTP frame.
func resolveRuntimeDTMF(ctx context.Context, digit string) (byte, error) {
	digit = strings.TrimSpace(digit)
	if socket := os.Getenv("PROLETARIUS_CARRIER_SOCKET"); socket != "" {
		payload, err := json.Marshal(map[string]string{"digit": digit})
		if err != nil {
			return 0, err
		}
		var result struct {
			Digit string `json:"digit"`
			Event int    `json:"event"`
		}
		if err := requestNativeJSON(ctx, socket, "/api/v2/call/dtmf", payload, &result); err != nil {
			return 0, err
		}
		if result.Digit != digit || result.Event < 0 || result.Event > 11 {
			return 0, fmt.Errorf("Rust DTMF 事件无效")
		}
		return byte(result.Event), nil
	}
	switch digit {
	case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9":
		return digit[0] - '0', nil
	case "*":
		return 10, nil
	case "#":
		return 11, nil
	default:
		return 0, fmt.Errorf("DTMF 仅支持 0-9、* 和 #")
	}
}
