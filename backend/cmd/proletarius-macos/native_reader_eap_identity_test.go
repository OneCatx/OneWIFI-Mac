package main

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNativeEAPIdentityUsesAuthenticatedSocketAndPreservesIdentity(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "reader-eap-id-")
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
	t.Setenv("PROLETARIUS_APP_TOKEN", "identity-token")
	identityRequest := []byte{1, 17, 0, 5, 1}
	identity := []byte("nai@example")
	responsePacket := append([]byte{2, 17, 0, 16, 1}, identity...)
	transcript := append(append([]byte(nil), identityRequest...), responsePacket...)
	checkcode := sha1.Sum(transcript)
	exchangeID := "abababababababababababababababababababababababababababababababab"
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v2/vowifi/eap-identity" ||
			request.Header.Get("X-Proletarius-App-Token") != "identity-token" {
			http.Error(w, "invalid route", http.StatusUnauthorized)
			return
		}
		var input struct {
			Identity []int `json:"identity"`
			Request  []int `json:"request"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil ||
			!reflect.DeepEqual(input.Identity, byteInts(identity)) ||
			!reflect.DeepEqual(input.Request, byteInts(identityRequest)) {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		response, _ := json.Marshal(map[string]any{
			"eap_response": byteInts(responsePacket), "exchange_id": exchangeID,
			"checkcode": byteInts(checkcode[:]),
		})
		_, _ = w.Write(response)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	result, handled, err := respondRuntimeEAPIdentity(context.Background(), identityRequest, identity)
	if err != nil || !handled || !reflect.DeepEqual(result.Packet, responsePacket) ||
		result.ExchangeID != exchangeID || !reflect.DeepEqual(result.Checkcode, checkcode[:]) {
		t.Fatalf("invalid Rust EAP Identity response: %#v handled=%v err=%v", result, handled, err)
	}
}

func TestNativeEAPIdentityRejectsMalformedRequestBeforeSocketIO(t *testing.T) {
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	_, handled, err := respondRuntimeEAPIdentity(context.Background(), []byte{1, 8, 0, 5, 23}, []byte("nai@example"))
	if !handled || err == nil || !strings.Contains(err.Error(), "请求无效") {
		t.Fatalf("malformed Identity request was not rejected before socket I/O: handled=%v err=%v", handled, err)
	}
}

func TestNativeEAPIdentityRejectsWrongIdentifierOrIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		packet []byte
	}{
		{name: "identifier", packet: []byte{2, 18, 0, 16, 1, 'n', 'a', 'i', '@', 'e', 'x', 'a', 'm', 'p', 'l', 'e'}},
		{name: "identity", packet: []byte{2, 17, 0, 16, 1, 'N', 'a', 'i', '@', 'e', 'x', 'a', 'm', 'p', 'l', 'e'}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory, err := os.MkdirTemp("/tmp", "reader-eap-id-bad-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
			socket := filepath.Join(directory, "r.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
			t.Setenv("PROLETARIUS_APP_TOKEN", "identity-token")
			var released atomic.Int32
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/api/v2/vowifi/eap-exchange/release" {
					var input struct {
						ExchangeID string `json:"exchange_id"`
					}
					if json.NewDecoder(request.Body).Decode(&input) != nil || input.ExchangeID != testNativeEAPExchangeID {
						http.Error(w, "invalid release", http.StatusBadRequest)
						return
					}
					released.Add(1)
					_, _ = w.Write([]byte("{}"))
					return
				}
				response, _ := json.Marshal(map[string]any{
					"eap_response": byteInts(test.packet),
					"exchange_id":  "abababababababababababababababababababababababababababababababab",
					"checkcode":    make([]int, 20),
				})
				_, _ = w.Write(response)
			})}
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() { _ = server.Close() })

			_, handled, err := respondRuntimeEAPIdentity(
				context.Background(), []byte{1, 17, 0, 5, 1}, []byte("nai@example"),
			)
			if !handled || err == nil || !strings.Contains(err.Error(), "与请求不匹配") {
				t.Fatalf("mismatched Identity response was accepted: handled=%v err=%v", handled, err)
			}
			if released.Load() != 1 {
				t.Fatal("invalid native response did not release its exchange")
			}
		})
	}
}
