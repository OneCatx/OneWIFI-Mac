package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func validateRuntimeIMSRegisterResponse(ctx context.Context, response []byte, callID string, cseq uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(response) == 0 || strings.TrimSpace(callID) == "" || cseq == 0 {
		return fmt.Errorf("IMS REGISTER 响应事务参数无效")
	}
	socket := os.Getenv("PROLETARIUS_CARRIER_SOCKET")
	if socket == "" {
		return nil
	}
	octets := make([]int, len(response))
	for index, value := range response {
		octets[index] = int(value)
	}
	payload, err := json.Marshal(map[string]any{"response": octets, "call_id": strings.TrimSpace(callID), "cseq": cseq})
	if err != nil {
		return err
	}
	var result struct {
		Status int    `json:"status"`
		CallID string `json:"call_id"`
		CSeq   uint32 `json:"cseq"`
	}
	if err := requestNativeJSON(ctx, socket, "/api/v2/ims/validate-register-response", payload, &result); err != nil {
		return err
	}
	if result.Status < 100 || result.Status > 699 || result.CallID != strings.TrimSpace(callID) || result.CSeq != cseq {
		return fmt.Errorf("Rust IMS REGISTER 响应摘要不一致")
	}
	return nil
}
