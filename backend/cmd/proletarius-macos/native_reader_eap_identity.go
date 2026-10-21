package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/1239t/swu-go/pkg/eap"
)

type nativeEAPIdentityResponse struct {
	Packet     []byte
	ExchangeID string
	Checkcode  []byte
}

func respondRuntimeEAPIdentity(
	ctx context.Context,
	identityRequest, identity []byte,
) (result nativeEAPIdentityResponse, handled bool, err error) {
	socket := os.Getenv("PROLETARIUS_CARRIER_SOCKET")
	if socket == "" {
		return nativeEAPIdentityResponse{}, false, nil
	}
	result, err = respondRuntimeEAPIdentityAtSocket(ctx, socket, identityRequest, identity)
	return result, true, err
}

// respondRuntimeEAPIdentityAtSocket keeps an EAP Identity transcript attached
// to the sidecar selected at session creation.  A native session must never
// re-read a mutable process setting and accidentally create its exchange in a
// different sidecar.
func respondRuntimeEAPIdentityAtSocket(
	ctx context.Context,
	socket string,
	identityRequest, identity []byte,
) (result nativeEAPIdentityResponse, err error) {
	socket = strings.TrimSpace(socket)
	if socket == "" {
		return nativeEAPIdentityResponse{}, fmt.Errorf("Rust EAP Identity 会话缺少本机服务 socket")
	}
	if len(identity) == 0 || len(identity) > 2048 || len(identityRequest) < 5 || len(identityRequest) > 4096 {
		return nativeEAPIdentityResponse{}, fmt.Errorf("Rust EAP Identity 输入无效")
	}
	requestPacket, parseErr := eap.Parse(identityRequest)
	if parseErr != nil || !sameDeclaredEAPLength(identityRequest) ||
		requestPacket.Code != eap.CodeRequest || requestPacket.Type != eap.TypeIdentity {
		return nativeEAPIdentityResponse{}, fmt.Errorf("Rust EAP Identity 请求无效")
	}
	payload, marshalErr := json.Marshal(map[string]any{
		"identity": byteInts(identity),
		"request":  byteInts(identityRequest),
	})
	if marshalErr != nil {
		return nativeEAPIdentityResponse{}, marshalErr
	}
	var response struct {
		EAPResponse []int  `json:"eap_response"`
		ExchangeID  string `json:"exchange_id"`
		Checkcode   []int  `json:"checkcode"`
	}
	if err := requestNativeJSONWithTimeout(
		ctx,
		socket,
		"/api/v2/vowifi/eap-identity",
		payload,
		&response,
		5*time.Second,
	); err != nil {
		return nativeEAPIdentityResponse{}, err
	}
	// Invalid native responses must not strand the newly created exchange.
	defer func() {
		if err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = releaseRuntimeEAPIdentityExchangeAtSocket(cleanupCtx, socket, response.ExchangeID)
		}
	}()
	packet, err := checkedOctets(response.EAPResponse, 5, 4096)
	if err != nil {
		return nativeEAPIdentityResponse{}, fmt.Errorf("Rust EAP Identity 响应包无效")
	}
	parsed, parseErr := eap.Parse(packet)
	if parseErr != nil || !sameDeclaredEAPLength(packet) ||
		parsed.Code != eap.CodeResponse || parsed.Identifier != requestPacket.Identifier ||
		parsed.Type != eap.TypeIdentity || !bytes.Equal(parsed.Data, identity) {
		return nativeEAPIdentityResponse{}, fmt.Errorf("Rust EAP Identity 响应与请求不匹配")
	}
	checkcode, err := checkedOctets(response.Checkcode, 20, 20)
	if err != nil || !validNativeEAPExchangeID(response.ExchangeID) {
		return nativeEAPIdentityResponse{}, fmt.Errorf("Rust EAP Identity exchange 元数据无效")
	}
	return nativeEAPIdentityResponse{Packet: packet, ExchangeID: response.ExchangeID, Checkcode: checkcode}, nil
}

func releaseRuntimeEAPIdentityExchange(ctx context.Context, exchangeID string) error {
	if !validNativeEAPExchangeID(exchangeID) {
		return nil
	}
	socket := os.Getenv("PROLETARIUS_CARRIER_SOCKET")
	if socket == "" {
		return nil
	}
	return releaseRuntimeEAPIdentityExchangeAtSocket(ctx, socket, exchangeID)
}

func releaseRuntimeEAPIdentityExchangeAtSocket(ctx context.Context, socket, exchangeID string) error {
	if !validNativeEAPExchangeID(exchangeID) {
		return nil
	}
	socket = strings.TrimSpace(socket)
	if socket == "" {
		return nil
	}
	payload, err := json.Marshal(map[string]string{"exchange_id": exchangeID})
	if err != nil {
		return err
	}
	return requestNativeJSONWithTimeout(
		ctx,
		socket,
		"/api/v2/vowifi/eap-exchange/release",
		payload,
		&struct{}{},
		2*time.Second,
	)
}

func validNativeEAPExchangeID(exchangeID string) bool {
	if len(exchangeID) != 64 {
		return false
	}
	for _, value := range exchangeID {
		if (value < '0' || value > '9') && (value < 'a' || value > 'f') {
			return false
		}
	}
	return true
}
