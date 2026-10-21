package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"proletarius.local/core/internal/vowifi/imscore"
)

func requireNativeSMSCodecSocket() (string, error) {
	socket := strings.TrimSpace(os.Getenv("PROLETARIUS_CARRIER_SOCKET"))
	if socket == "" {
		return "", fmt.Errorf("Rust SMS codec service is unavailable")
	}
	return socket, nil
}

func decodeRuntimeSMSDeliver(ctx context.Context, tpdu []byte) (imscore.InboundSMS, bool, error) {
	if len(tpdu) == 0 {
		return imscore.InboundSMS{}, false, nil
	}
	messageType := tpdu[0] & 0x03
	if messageType != 0 && messageType != 2 {
		return imscore.InboundSMS{}, false, nil
	}
	if err := ctx.Err(); err != nil {
		return imscore.InboundSMS{}, false, err
	}
	socket, err := requireNativeSMSCodecSocket()
	if err != nil {
		return imscore.InboundSMS{}, false, err
	}
	octets := make([]int, len(tpdu))
	for index, value := range tpdu {
		octets[index] = int(value)
	}
	payload, err := json.Marshal(map[string]any{"tpdu": octets})
	if err != nil {
		return imscore.InboundSMS{}, false, err
	}
	var result struct {
		Kind              string  `json:"kind"`
		Sender            string  `json:"sender"`
		DeliveryStatus    string  `json:"delivery_status"`
		StatusCode        int     `json:"status_code"`
		Text              *string `json:"text"`
		Binary            []byte  `json:"binary"`
		ServiceCenterTime *struct {
			Year          int `json:"year"`
			Month         int `json:"month"`
			Day           int `json:"day"`
			Hour          int `json:"hour"`
			Minute        int `json:"minute"`
			Second        int `json:"second"`
			OffsetMinutes int `json:"offset_minutes"`
		} `json:"service_center_time"`
	}
	if err := requestNativeJSON(ctx, socket, "/api/v2/sms/decode-incoming-tpdu", payload, &result); err != nil {
		return imscore.InboundSMS{}, false, err
	}
	if result.Kind == "status_report" {
		if messageType != 2 || result.StatusCode < 0 || result.StatusCode > 255 || !validSMSDeliveryStatus(result.DeliveryStatus, byte(result.StatusCode)) {
			return imscore.InboundSMS{}, false, fmt.Errorf("Rust 短信状态报告响应无效")
		}
		return imscore.InboundSMS{StatusReport: true, DeliveryStatus: result.DeliveryStatus, StatusCode: byte(result.StatusCode)}, true, nil
	}
	if messageType != 0 || result.Kind != "deliver" || strings.TrimSpace(result.Sender) == "" || result.Text == nil || result.Binary != nil {
		return imscore.InboundSMS{}, false, fmt.Errorf("Rust 接收短信正文响应无效")
	}
	decoded := imscore.InboundSMS{Sender: result.Sender, Content: *result.Text}
	if value := result.ServiceCenterTime; value != nil {
		if value.Year < 1900 || value.Year > 2099 || value.Month < 1 || value.Month > 12 || value.Day < 1 || value.Day > 31 || value.Hour < 0 || value.Hour > 23 || value.Minute < 0 || value.Minute > 59 || value.Second < 0 || value.Second > 59 || value.OffsetMinutes < -840 || value.OffsetMinutes > 840 {
			return imscore.InboundSMS{}, false, fmt.Errorf("Rust 接收短信时间无效")
		}
		decoded.Timestamp = time.Date(value.Year, time.Month(value.Month), value.Day, value.Hour, value.Minute, value.Second, 0, time.FixedZone("SMS", value.OffsetMinutes*60))
		if decoded.Timestamp.Year() != value.Year || int(decoded.Timestamp.Month()) != value.Month || decoded.Timestamp.Day() != value.Day {
			return imscore.InboundSMS{}, false, fmt.Errorf("Rust 接收短信日期无效")
		}
		decoded.HasTimestamp = true
	}
	return decoded, true, nil
}

func validSMSDeliveryStatus(status string, code byte) bool {
	expected := "reserved"
	switch {
	case code <= 0x1f:
		expected = "delivered"
	case code <= 0x3f:
		expected = "temporary_error"
	case code <= 0x5f:
		expected = "permanent_error"
	case code <= 0x7f:
		expected = "temporary_error_no_retry"
	}
	return status == expected
}

func prepareRuntimeTPDUs(ctx context.Context, recipient, text string, reference byte) ([][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	socket, err := requireNativeSMSCodecSocket()
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{"recipient": recipient, "text": text, "reference": reference})
	if err != nil {
		return nil, err
	}
	var result struct {
		TPDUs [][]byte `json:"tpdus"`
	}
	if err := requestNativeJSON(ctx, socket, "/api/v2/sms/prepare-tpdu", payload, &result); err != nil {
		return nil, err
	}
	if len(result.TPDUs) == 0 || len(result.TPDUs) > 255 {
		return nil, fmt.Errorf("Rust 短信分段数量无效")
	}
	for _, part := range result.TPDUs {
		if len(part) < 8 || len(part) > 232 || part[0]&3 != 1 {
			return nil, fmt.Errorf("Rust 短信 TPDU 无效")
		}
	}
	return result.TPDUs, nil
}

func encodeRuntimeRPDU(ctx context.Context, reference byte, smsc string, tpdu []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	socket, err := requireNativeSMSCodecSocket()
	if err != nil {
		return nil, err
	}
	// JSON byte slices use base64; explicitly encode octets as an integer array.
	octets := make([]int, len(tpdu))
	for i, b := range tpdu {
		octets[i] = int(b)
	}
	payload, err := json.Marshal(map[string]any{"reference": reference, "smsc": smsc, "tpdu": octets})
	if err != nil {
		return nil, err
	}
	var result struct {
		RPDU []byte `json:"rpdu"`
	}
	if err := requestNativeJSON(ctx, socket, "/api/v2/sms/encode-rpdu", payload, &result); err != nil {
		return nil, err
	}
	if !validNativeRPDU(result.RPDU, reference, tpdu) {
		return nil, fmt.Errorf("Rust 短信封装响应无效")
	}
	return result.RPDU, nil
}

// Check framing and payload identity without reimplementing the Rust encoder.
func validNativeRPDU(body []byte, reference byte, tpdu []byte) bool {
	if len(body) < 5 || len(body) > 255 || body[0] != 0 || body[1] != reference || body[2] != 0 {
		return false
	}
	addressLength := int(body[3])
	lengthOffset := 4 + addressLength
	if addressLength < 3 || addressLength > 11 || lengthOffset >= len(body) {
		return false
	}
	return len(tpdu) > 0 && len(tpdu) <= 232 && int(body[lengthOffset]) == len(tpdu) && bytes.Equal(body[lengthOffset+1:], tpdu)
}

func decodeRuntimeRPDU(ctx context.Context, body []byte) (byte, string, []byte, error) {
	if err := ctx.Err(); err != nil {
		return 0, "", nil, err
	}
	socket, err := requireNativeSMSCodecSocket()
	if err != nil {
		return 0, "", nil, err
	}
	if len(body) < 5 || len(body) > 255 || body[0] != 1 {
		return 0, "", nil, fmt.Errorf("接收短信 RP-DATA 无效")
	}
	octets := make([]int, len(body))
	for i, b := range body {
		octets[i] = int(b)
	}
	payload, err := json.Marshal(map[string]any{"rpdu": octets})
	if err != nil {
		return 0, "", nil, err
	}
	var result struct {
		Reference  byte   `json:"reference"`
		Originator string `json:"originator"`
		TPDU       []byte `json:"tpdu"`
	}
	if err := requestNativeJSON(ctx, socket, "/api/v2/sms/decode-rpdu", payload, &result); err != nil {
		return 0, "", nil, err
	}
	if !validNativeIncomingRPDU(body, result.Reference, result.TPDU) {
		return 0, "", nil, fmt.Errorf("Rust 接收短信解析响应无效")
	}
	return result.Reference, result.Originator, result.TPDU, nil
}

func validNativeIncomingRPDU(body []byte, reference byte, tpdu []byte) bool {
	if len(body) < 6 || len(body) > 255 || body[0] != 1 || body[1] != reference || len(tpdu) == 0 || len(tpdu) > 232 {
		return false
	}
	offset := 2
	for field := 0; field < 2; field++ {
		if offset >= len(body) {
			return false
		}
		offset += 1 + int(body[offset])
	}
	return offset < len(body) && int(body[offset]) == len(tpdu) && bytes.Equal(body[offset+1:], tpdu)
}
