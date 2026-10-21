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

type nativeReaderIdentity struct {
	ICCID string `json:"iccid"`
	IMSI  string `json:"imsi"`
	MCC   string `json:"mcc"`
	MNC   string `json:"mnc"`
}

// readRuntimeReaderIdentity asks the authenticated Rust gateway for stable,
// read-only USIM identity fields. An empty AID asks Rust to discover the
// complete USIM AID from EF-DIR. handled is false only when the gateway is
// not configured; callers can then safely retain compatibility values.
func readRuntimeReaderIdentity(ctx context.Context, reader string, aid []byte) (nativeReaderIdentity, bool, error) {
	socket := os.Getenv("PROLETARIUS_CARRIER_SOCKET")
	if socket == "" {
		return nativeReaderIdentity{}, false, nil
	}
	if strings.TrimSpace(reader) == "" || (len(aid) != 0 && (len(aid) < 5 || len(aid) > 16)) {
		return nativeReaderIdentity{}, true, fmt.Errorf("Rust 读卡器身份输入无效")
	}
	var response struct {
		OK bool `json:"ok"`
		nativeReaderIdentity
	}
	payload, err := json.Marshal(map[string]any{
		"reader": strings.TrimSpace(reader),
		"aid":    byteInts(aid),
	})
	if err != nil {
		return nativeReaderIdentity{}, true, err
	}
	if err := requestNativeJSONWithTimeout(ctx, socket, "/api/v2/reader/identity", payload, &response, 20*time.Second); err != nil {
		var failure *nativeHTTPError
		if errors.As(err, &failure) {
			switch failure.code {
			case "reader_busy":
				return nativeReaderIdentity{}, true, errReaderBusy
			case "reader_card_absent":
				return nativeReaderIdentity{}, true, errReaderCardAbsent
			case "reader_unavailable":
				return nativeReaderIdentity{}, true, errReaderUnavailable
			}
		}
		return nativeReaderIdentity{}, true, err
	}
	identity := nativeReaderIdentity{
		ICCID: strings.TrimSpace(response.ICCID),
		IMSI:  strings.TrimSpace(response.IMSI),
		MCC:   strings.TrimSpace(response.MCC),
		MNC:   strings.TrimSpace(response.MNC),
	}
	if !response.OK || len(identity.ICCID) < 10 || len(identity.ICCID) > 20 {
		return nativeReaderIdentity{}, true, fmt.Errorf("Rust ICCID 响应无效")
	}
	for _, digit := range identity.ICCID {
		if digit < '0' || digit > '9' {
			return nativeReaderIdentity{}, true, fmt.Errorf("Rust ICCID 包含非法字符")
		}
	}
	if identity.IMSI != "" && (len(identity.IMSI) < 6 || len(identity.IMSI) > 15) {
		return nativeReaderIdentity{}, true, fmt.Errorf("Rust IMSI 响应无效")
	}
	return identity, true, nil
}

func readRuntimeReaderICCID(ctx context.Context, reader string, aid []byte) (string, bool, error) {
	identity, handled, err := readRuntimeReaderIdentity(ctx, reader, aid)
	return identity.ICCID, handled, err
}
