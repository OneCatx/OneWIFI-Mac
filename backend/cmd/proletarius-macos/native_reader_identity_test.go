package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeIdentityPreservesReaderFailureAcrossSocket(t *testing.T) {
	for _, test := range []struct {
		code   string
		status int
		want   error
	}{
		{"reader_busy", http.StatusServiceUnavailable, errReaderBusy},
		{"reader_card_absent", http.StatusConflict, errReaderCardAbsent},
		{"reader_unavailable", http.StatusConflict, errReaderUnavailable},
	} {
		t.Run(test.code, func(t *testing.T) {
			directory, err := os.MkdirTemp("/tmp", "identity-failure-")
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
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": test.code})
			})}
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() { _ = server.Close() })
			_, handled, err := readRuntimeReaderIdentity(context.Background(), "Reader", nil)
			if !handled || !errors.Is(err, test.want) {
				t.Fatalf("handled=%v error=%v", handled, err)
			}
		})
	}
}

func TestNativeReaderIdentityDiscoversMissingAID(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pc-reader-identity-")
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
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v2/reader/identity" {
			http.Error(w, "invalid route", http.StatusBadRequest)
			return
		}
		var input struct {
			Reader string          `json:"reader"`
			AID    json.RawMessage `json:"aid"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.Reader != "Fixture Reader" || string(input.AID) != "[]" {
			http.Error(w, "AID was not omitted", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"iccid":"8901234567890123456","imsi":"234100896392986","mcc":"234","mnc":""}`))
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	identity, handled, err := readRuntimeReaderIdentity(context.Background(), " Fixture Reader ", nil)
	if err != nil || !handled || identity.ICCID != "8901234567890123456" || identity.MNC != "" {
		t.Fatalf("unexpected native identity: handled=%v err=%v", handled, err)
	}
}
