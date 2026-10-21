package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/1239t/swu-go/pkg/eap"
)

type nativeEAPAKAResponse struct {
	NextExchangeID string
	Outcome        string
	Packet         []byte
	MSK            []byte
	KAut           []byte
}

// nativeReaderEAPAKAHTTPTimeout intentionally exceeds the Rust reader-task
// deadline.  The native worker must have enough time to return its explicit
// timeout/error response before Go tears down the Unix-socket request; equal
// deadlines can otherwise turn a completed physical AKA into an ambiguous
// client-side timeout.
const nativeReaderEAPAKAHTTPTimeout = 35 * time.Second

func respondRuntimeReaderEAPAKA(
	ctx context.Context,
	reader, application string,
	exchangeID string,
	challenge []byte,
	checkcodeMode string,
) (nativeEAPAKAResponse, bool, error) {
	return respondRuntimeReaderEAPAKAForIdentity(ctx, reader, application, exchangeID, challenge, checkcodeMode, nil)
}

func respondRuntimeReaderEAPAKAForIdentity(
	ctx context.Context, reader, application, exchangeID string,
	challenge []byte, checkcodeMode string, expected *nativeReaderAuthIdentity,
) (nativeEAPAKAResponse, bool, error) {
	socket := os.Getenv("PROLETARIUS_CARRIER_SOCKET")
	if socket == "" {
		return nativeEAPAKAResponse{}, false, nil
	}
	result, err := respondRuntimeReaderEAPAKAForIdentityAtSocket(
		ctx,
		socket,
		reader,
		application,
		exchangeID,
		challenge,
		checkcodeMode,
		expected,
	)
	return result, true, err
}

// respondRuntimeReaderEAPAKAForIdentityAtSocket uses the socket chosen by a
// reader VoWiFi session.  It deliberately has no compatibility fallback: a
// session that selected the native standard EAP-AKA owner must fail rather
// than run a second physical AKA transaction through Go.
func respondRuntimeReaderEAPAKAForIdentityAtSocket(
	ctx context.Context,
	socket string,
	reader, application, exchangeID string,
	challenge []byte,
	checkcodeMode string,
	expected *nativeReaderAuthIdentity,
) (nativeEAPAKAResponse, error) {
	socket = strings.TrimSpace(socket)
	if socket == "" {
		return nativeEAPAKAResponse{}, fmt.Errorf("Rust EAP-AKA 会话缺少本机服务 socket")
	}
	reader = strings.TrimSpace(reader)
	if reader == "" || (application != "usim" && application != "isim") ||
		!validNativeEAPExchangeID(exchangeID) ||
		len(challenge) < 5 || len(challenge) > 4096 ||
		!validAKAChallengeMode(checkcodeMode) {
		return nativeEAPAKAResponse{}, fmt.Errorf("Rust EAP-AKA 输入无效")
	}
	challengePacket, err := eap.Parse(challenge)
	if err != nil || !sameDeclaredEAPLength(challenge) ||
		challengePacket.Code != eap.CodeRequest || challengePacket.Type != eap.TypeAKA ||
		challengePacket.Subtype != eap.SubtypeChallenge {
		return nativeEAPAKAResponse{}, fmt.Errorf("Rust EAP-AKA challenge 无效")
	}
	payload := map[string]any{
		"reader":         reader,
		"application":    application,
		"exchange_id":    exchangeID,
		"challenge":      byteInts(challenge),
		"checkcode_mode": checkcodeMode,
	}
	if expected != nil {
		payload["expected_identity"] = expected
	}
	request, err := json.Marshal(payload)
	if err != nil {
		return nativeEAPAKAResponse{}, err
	}
	var response struct {
		NextExchangeID string `json:"next_exchange_id"`
		Outcome        string `json:"outcome"`
		EAPResponse    []int  `json:"eap_response"`
		MSK            []int  `json:"msk"`
		KAut           []int  `json:"k_aut"`
	}
	if err := requestNativeJSONWithTimeout(ctx, socket, "/api/v2/reader/eap-aka", request, &response, nativeReaderEAPAKAHTTPTimeout); err != nil {
		var nativeError *nativeHTTPError
		if errors.As(err, &nativeError) {
			switch nativeError.code {
			case "reader_card_absent":
				return nativeEAPAKAResponse{}, errReaderCardAbsent
			case "reader_unavailable":
				return nativeEAPAKAResponse{}, errReaderUnavailable
			case "reader_busy":
				return nativeEAPAKAResponse{}, errReaderBusy
			case "reader_authentication_rejected":
				return nativeEAPAKAResponse{}, errors.New("SIM 卡拒绝鉴权 (Authentication Reject)")
			case "reader_application_unavailable":
				if application == "isim" && nativeError.status == 404 {
					return nativeEAPAKAResponse{}, errReaderISIMUnavailable
				}
			}
		}
		return nativeEAPAKAResponse{}, err
	}
	keepNextExchange := false
	defer func() {
		if !keepNextExchange && response.NextExchangeID != "" {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = releaseRuntimeEAPIdentityExchangeAtSocket(cleanupCtx, socket, response.NextExchangeID)
		}
	}()
	packet, err := checkedOctets(response.EAPResponse, 5, 4096)
	if err != nil {
		return nativeEAPAKAResponse{}, fmt.Errorf("Rust EAP-AKA 响应包无效")
	}
	parsed, err := eap.Parse(packet)
	if err != nil || parsed.Code != eap.CodeResponse || parsed.Identifier != challenge[1] ||
		!sameDeclaredEAPLength(packet) || parsed.Type != eap.TypeAKA {
		return nativeEAPAKAResponse{}, fmt.Errorf("Rust EAP-AKA 响应与请求不匹配")
	}
	result := nativeEAPAKAResponse{Outcome: response.Outcome, Packet: packet, NextExchangeID: response.NextExchangeID}
	switch response.Outcome {
	case "success":
		if parsed.Subtype != eap.SubtypeChallenge || len(response.MSK) != 64 || len(response.KAut) != 16 || response.NextExchangeID != "" {
			return nativeEAPAKAResponse{}, fmt.Errorf("Rust EAP-AKA 成功响应缺少有效密钥或 Challenge")
		}
		result.MSK, err = checkedOctets(response.MSK, 64, 64)
		if err != nil {
			return nativeEAPAKAResponse{}, fmt.Errorf("Rust EAP-AKA MSK 无效")
		}
		result.KAut, err = checkedOctets(response.KAut, 16, 16)
		if err != nil {
			return nativeEAPAKAResponse{}, fmt.Errorf("Rust EAP-AKA K_aut 无效")
		}
	case "synchronization_failure":
		if parsed.Subtype != eap.SubtypeSyncFailure || len(response.MSK)+len(response.KAut) != 0 ||
			!validNativeEAPExchangeID(response.NextExchangeID) || response.NextExchangeID == exchangeID {
			return nativeEAPAKAResponse{}, fmt.Errorf("Rust EAP-AKA 同步失败响应无效")
		}
		keepNextExchange = true
	case "authentication_reject":
		return nativeEAPAKAResponse{}, errors.New("SIM 卡拒绝鉴权 (Authentication Reject)")
	default:
		return nativeEAPAKAResponse{}, fmt.Errorf("Rust EAP-AKA 返回未知结果")
	}
	return result, nil
}

func validAKAChallengeMode(mode string) bool {
	switch mode {
	case "minimal", "omit", "checkcode", "recompute":
		return true
	default:
		return false
	}
}

func sameDeclaredEAPLength(packet []byte) bool {
	return len(packet) >= 4 && int(packet[2])<<8|int(packet[3]) == len(packet)
}
