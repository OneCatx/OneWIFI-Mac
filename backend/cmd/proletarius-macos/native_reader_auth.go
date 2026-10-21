package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

func authenticateRuntimeReaderAKA(ctx context.Context, reader string, aid, rand, autn []byte) (*akaResult, bool, error) {
	return authenticateRuntimeReaderAKARequest(ctx, reader, aid, "", rand, autn, nil)
}

type nativeReaderAuthIdentity struct {
	ICCID string `json:"iccid"`
	IMSI  string `json:"imsi"`
}

func authenticateRuntimeReaderAKAAuto(ctx context.Context, reader, application string, rand, autn []byte, expected *nativeReaderAuthIdentity) (*akaResult, bool, error) {
	return authenticateRuntimeReaderAKARequest(ctx, reader, nil, application, rand, autn, expected)
}

func authenticateRuntimeReaderAKARequest(ctx context.Context, reader string, aid []byte, application string, rand, autn []byte, expected *nativeReaderAuthIdentity) (*akaResult, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	socket := os.Getenv("PROLETARIUS_CARRIER_SOCKET")
	if socket == "" {
		return nil, false, nil
	}
	validSelection := (application == "" && len(aid) >= 5 && len(aid) <= 16) ||
		((application == "usim" || application == "isim") && len(aid) == 0)
	if strings.TrimSpace(reader) == "" || !validSelection || len(rand) != 16 || len(autn) != 16 {
		return nil, true, fmt.Errorf("Rust 读卡器 AKA 输入无效")
	}
	request := map[string]any{
		"reader": strings.TrimSpace(reader),
		"rand":   byteInts(rand),
		"autn":   byteInts(autn),
	}
	if expected != nil {
		request["expected_identity"] = expected
	}
	if application == "" {
		request["aid"] = byteInts(aid)
	} else {
		request["application"] = application
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, true, err
	}
	var response struct {
		Outcome string `json:"outcome"`
		RES     []int  `json:"res"`
		CK      []int  `json:"ck"`
		IK      []int  `json:"ik"`
		AUTS    []int  `json:"auts"`
	}
	if err := requestNativeJSONWithTimeout(ctx, socket, "/api/v2/reader/authenticate", payload, &response, 25*time.Second); err != nil {
		var nativeError *nativeHTTPError
		if errors.As(err, &nativeError) {
			switch nativeError.code {
			case "reader_card_absent":
				return nil, true, errReaderCardAbsent
			case "reader_unavailable":
				return nil, true, errReaderUnavailable
			case "reader_busy":
				return nil, true, errReaderBusy
			case "reader_application_unavailable":
				if application == "isim" && nativeError.status == 404 {
					return nil, true, errReaderISIMUnavailable
				}
			}
		}
		return nil, true, err
	}
	switch response.Outcome {
	case "success":
		res, err := checkedOctets(response.RES, 4, 16)
		if err != nil {
			return nil, true, fmt.Errorf("Rust AKA RES 无效: %w", err)
		}
		ck, err := checkedOctets(response.CK, 16, 16)
		if err != nil {
			return nil, true, fmt.Errorf("Rust AKA CK 无效: %w", err)
		}
		ik, err := checkedOctets(response.IK, 16, 16)
		if err != nil {
			return nil, true, fmt.Errorf("Rust AKA IK 无效: %w", err)
		}
		if len(response.AUTS) != 0 {
			return nil, true, fmt.Errorf("Rust AKA 成功响应包含意外 AUTS")
		}
		return &akaResult{RES: res, CK: ck, IK: ik}, true, nil
	case "synchronization_failure":
		auts, err := checkedOctets(response.AUTS, 14, 14)
		if err != nil || len(response.RES)+len(response.CK)+len(response.IK) != 0 {
			return nil, true, fmt.Errorf("Rust AKA 同步失败响应无效")
		}
		return &akaResult{AUTS: auts}, true, errReaderAKASyncFailure
	case "authentication_reject":
		if len(response.RES)+len(response.CK)+len(response.IK)+len(response.AUTS) != 0 {
			return nil, true, fmt.Errorf("Rust AKA 拒绝响应包含意外密钥材料")
		}
		return nil, true, errors.New("SIM 卡拒绝鉴权 (Authentication Reject)")
	default:
		return nil, true, fmt.Errorf("Rust AKA 返回未知结果")
	}
}

func byteInts(value []byte) []int {
	result := make([]int, len(value))
	for index, octet := range value {
		result[index] = int(octet)
	}
	return result
}

func checkedOctets(value []int, minimum, maximum int) ([]byte, error) {
	if len(value) < minimum || len(value) > maximum {
		return nil, fmt.Errorf("长度 %d 不在 %d..%d", len(value), minimum, maximum)
	}
	result := make([]byte, len(value))
	for index, octet := range value {
		if octet < 0 || octet > 255 {
			return nil, fmt.Errorf("字节 %d 越界", index)
		}
		result[index] = byte(octet)
	}
	return result, nil
}
