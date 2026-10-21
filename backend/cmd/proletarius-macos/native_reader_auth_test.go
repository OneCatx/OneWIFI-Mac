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
	"testing"
)

func TestNativeReaderAKAUsesOctetArrays(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pc-reader-auth-")
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
	t.Setenv("PROLETARIUS_APP_TOKEN", "reader-token")

	aid := []byte{0xa0, 0x00, 0x00, 0x00, 0x87, 0x10, 0x02}
	rand := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	autn := []byte{16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v2/reader/authenticate" || request.Header.Get("X-Proletarius-App-Token") != "reader-token" {
			http.Error(w, "invalid route", http.StatusUnauthorized)
			return
		}
		var input struct {
			Reader string `json:"reader"`
			AID    []int  `json:"aid"`
			RAND   []int  `json:"rand"`
			AUTN   []int  `json:"autn"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil ||
			input.Reader != "Fixture Reader" ||
			!reflect.DeepEqual(input.AID, byteInts(aid)) ||
			!reflect.DeepEqual(input.RAND, byteInts(rand)) ||
			!reflect.DeepEqual(input.AUTN, byteInts(autn)) {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"outcome":"success","res":[1,2,3,4],"ck":[0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15],"ik":[16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31]}`))
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	result, handled, err := authenticateRuntimeReaderAKA(context.Background(), " Fixture Reader ", aid, rand, autn)
	if err != nil || !handled || !reflect.DeepEqual(result.RES, []byte{1, 2, 3, 4}) || len(result.CK) != 16 || len(result.IK) != 16 || len(result.AUTS) != 0 {
		t.Fatalf("unexpected native AKA result: %#v handled=%v err=%v", result, handled, err)
	}
}

func TestNativeReaderAKAOutcomesAreStrict(t *testing.T) {
	tests := []struct {
		name       string
		response   string
		wantSync   bool
		wantReject bool
	}{
		{name: "synchronization failure", response: `{"outcome":"synchronization_failure","auts":[0,1,2,3,4,5,6,7,8,9,10,11,12,13]}`, wantSync: true},
		{name: "authentication reject", response: `{"outcome":"authentication_reject"}`, wantReject: true},
		{name: "invalid octet", response: `{"outcome":"success","res":[1,2,3,256],"ck":[0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15],"ik":[16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31]}`},
		{name: "secret leak on reject", response: `{"outcome":"authentication_reject","res":[1,2,3,4]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory, err := os.MkdirTemp("/tmp", "pc-reader-outcome-")
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
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(test.response)) })}
			go func() { _ = server.Serve(listener) }()
			defer server.Close()

			result, handled, err := authenticateRuntimeReaderAKA(
				context.Background(), "Reader", []byte{1, 2, 3, 4, 5}, make([]byte, 16), make([]byte, 16))
			if !handled || err == nil {
				t.Fatalf("expected handled error, got result=%#v handled=%v err=%v", result, handled, err)
			}
			if test.wantSync && (!errors.Is(err, errReaderAKASyncFailure) || result == nil || len(result.AUTS) != 14) {
				t.Fatalf("invalid synchronization result: %#v %v", result, err)
			}
			if test.wantReject && result != nil {
				t.Fatalf("reject returned key material: %#v", result)
			}
		})
	}
}

func TestNativeReaderAKADisabledWithoutSocket(t *testing.T) {
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", "")
	result, handled, err := authenticateRuntimeReaderAKA(
		context.Background(), "Reader", []byte{1, 2, 3, 4, 5}, make([]byte, 16), make([]byte, 16))
	if result != nil || handled || err != nil {
		t.Fatalf("native bridge should be disabled: %#v %v %v", result, handled, err)
	}
}

func TestNativeReaderAKAAutoDiscoversISIMAndPreservesMissingFallback(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pc-reader-auto-")
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
		var input map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Errorf("decode AKA request: %v", err)
		}
		if _, hasAID := input["aid"]; hasAID || string(input["application"]) != `"isim"` {
			t.Errorf("auto AKA must send ISIM selector without a Go-discovered AID: %s", input)
		}
		var expected nativeReaderAuthIdentity
		if err := json.Unmarshal(input["expected_identity"], &expected); err != nil || expected.ICCID != "8901234567890123456" || expected.IMSI != "234100896392986" {
			t.Error("manager did not bind the selected SIM identity")
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"reader_application_unavailable"}`))
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	rm := &readerManager{readerName: "Reader", generation: 1, cachedUSIM: &usimInfo{ICCID: "8901234567890123456", IMSI: "234100896392986"}}
	result, err := rm.authenticateCoreAtGeneration(make([]byte, 16), make([]byte, 16), isimApplicationAIDPrefix, "ISIM", 1)
	if result != nil || !errors.Is(err, errReaderISIMUnavailable) {
		t.Fatalf("missing ISIM fallback lost: result=%#v err=%v", result, err)
	}
}

func TestNativeReaderAKAPreservesReaderStateFailures(t *testing.T) {
	for _, test := range []struct {
		code string
		want error
	}{
		{"reader_card_absent", errReaderCardAbsent},
		{"reader_unavailable", errReaderUnavailable},
		{"reader_busy", errReaderBusy},
	} {
		t.Run(test.code, func(t *testing.T) {
			directory, err := os.MkdirTemp("/tmp", "pc-reader-state-")
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
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"code":"` + test.code + `"}`))
			})}
			go func() { _ = server.Serve(listener) }()
			defer server.Close()

			_, handled, err := authenticateRuntimeReaderAKA(
				context.Background(), "Reader", []byte{1, 2, 3, 4, 5}, make([]byte, 16), make([]byte, 16))
			if !handled || !errors.Is(err, test.want) {
				t.Fatalf("code %s: handled=%v err=%v", test.code, handled, err)
			}
		})
	}
}
