package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNativeReaderAuthenticationStateCheckNeverWaitsForRefresh(t *testing.T) {
	rm := &readerManager{readerName: "Reader", generation: 7}
	rm.authMu.Lock()
	defer rm.authMu.Unlock()
	rm.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- rm.validateAuthenticationReader("Reader", 7) }()
	select {
	case err := <-done:
		rm.mu.Unlock()
		if !errors.Is(err, errReaderBusy) {
			t.Fatalf("expected busy during refresh, got %v", err)
		}
	case <-time.After(time.Second):
		rm.mu.Unlock()
		<-done
		t.Fatal("authentication waited for the refresh lock")
	}
	if err := rm.validateAuthenticationReader("Reader", 7); err != nil {
		t.Fatalf("same reader should recover after contention: %v", err)
	}
	for _, stale := range []struct {
		reader     string
		generation uint64
	}{{"Other", 7}, {"Reader", 6}} {
		if err := rm.validateAuthenticationReader(stale.reader, stale.generation); err == nil {
			t.Fatal("stale authentication accepted")
		}
	}
}

const testNativeEAPExchangeID = "abababababababababababababababababababababababababababababababab"

func TestNativeReaderEAPAKAUsesAuthenticatedSocketAndValidatesResponse(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pc-reader-eap-aka-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "reader.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	t.Setenv("PROLETARIUS_APP_TOKEN", "eap-token")

	challenge := []byte{1, 41, 0, 8, 23, 1, 0, 0}
	packet := []byte{2, 41, 0, 8, 23, 1, 0, 0}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v2/reader/eap-aka" || request.Header.Get("X-Proletarius-App-Token") != "eap-token" {
			http.Error(w, "invalid route", http.StatusUnauthorized)
			return
		}
		var input struct {
			Reader           string                    `json:"reader"`
			Application      string                    `json:"application"`
			ExchangeID       string                    `json:"exchange_id"`
			Challenge        []int                     `json:"challenge"`
			CheckcodeMode    string                    `json:"checkcode_mode"`
			ExpectedIdentity *nativeReaderAuthIdentity `json:"expected_identity"`
		}
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil ||
			input.Reader != "Fixture Reader" || input.Application != "usim" ||
			input.ExchangeID != testNativeEAPExchangeID ||
			input.CheckcodeMode != "recompute" ||
			input.ExpectedIdentity == nil || input.ExpectedIdentity.ICCID != "8901234567890123456" || input.ExpectedIdentity.IMSI != "234100896392986" ||
			!reflect.DeepEqual(input.Challenge, byteInts(challenge)) {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		response, _ := json.Marshal(map[string]any{
			"outcome": "success", "eap_response": byteInts(packet),
			"msk": make([]int, 64), "k_aut": make([]int, 16),
		})
		_, _ = w.Write(response)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	rm := &readerManager{readerName: "Fixture Reader", generation: 1, cachedUSIM: &usimInfo{ICCID: "8901234567890123456", IMSI: "234100896392986"}}
	result, handled, err := rm.respondEAPAKAAtGeneration(challenge, testNativeEAPExchangeID, "recompute", 1)
	if err != nil || !handled || result.Outcome != "success" || !reflect.DeepEqual(result.Packet, packet) || len(result.MSK) != 64 || len(result.KAut) != 16 {
		t.Fatalf("invalid Rust EAP-AKA result: %#v handled=%v err=%v", result, handled, err)
	}
}

func TestNativeReaderEAPAKAUnavailableSocketPreservesFallback(t *testing.T) {
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", "")
	result, handled, err := respondRuntimeReaderEAPAKA(context.Background(), "Reader", "usim", testNativeEAPExchangeID, []byte{1, 4, 0, 5, 23}, "minimal")
	if result.Packet != nil || handled || err != nil {
		t.Fatalf("unconfigured native EAP route should preserve fallback: %#v handled=%v err=%v", result, handled, err)
	}
	if validAKAChallengeMode("guess") || !validAKAChallengeMode("checkcode") {
		t.Fatal("checkcode mode validation is not fail-closed")
	}
}

func TestNativeReaderEAPAKARejectsMalformedChallengeBeforeSocketIO(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "missing.sock")
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	_, handled, err := respondRuntimeReaderEAPAKA(
		context.Background(), "Reader", "usim", testNativeEAPExchangeID,
		[]byte{1, 4, 0, 5, 23}, "minimal",
	)
	if !handled || err == nil || !strings.Contains(err.Error(), "challenge") {
		t.Fatalf("malformed challenge was not rejected before socket I/O: handled=%v err=%v", handled, err)
	}
}

func TestNativeReaderEAPAKARejectsResponseForAnotherChallenge(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "reader-eap-response-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "reader.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	t.Setenv("PROLETARIUS_APP_TOKEN", "eap-token")
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		response, _ := json.Marshal(map[string]any{
			"outcome": "success", "eap_response": byteInts([]byte{2, 42, 0, 8, 23, 1, 0, 0}),
			"msk": make([]int, 64), "k_aut": make([]int, 16),
		})
		_, _ = w.Write(response)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	_, handled, err := respondRuntimeReaderEAPAKA(
		context.Background(), "Reader", "usim",
		testNativeEAPExchangeID,
		[]byte{1, 41, 0, 8, 23, 1, 0, 0}, "minimal",
	)
	if !handled || err == nil || !strings.Contains(err.Error(), "与请求不匹配") {
		t.Fatalf("cross-challenge response was not rejected: handled=%v err=%v", handled, err)
	}
}
