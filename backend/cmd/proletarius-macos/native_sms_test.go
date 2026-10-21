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

func TestNativeSMSCodecFailsClosedWithoutRustGateway(t *testing.T) {
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", "")
	checks := []struct {
		name string
		run  func() error
	}{
		{
			name: "submit TPDU",
			run: func() error {
				_, err := prepareRuntimeTPDUs(context.Background(), "888", "BAL", 1)
				return err
			},
		},
		{
			name: "outgoing RPDU",
			run: func() error {
				_, err := encodeRuntimeRPDU(context.Background(), 1, "", []byte{1})
				return err
			},
		},
		{
			name: "incoming RPDU",
			run: func() error {
				_, _, _, err := decodeRuntimeRPDU(context.Background(), []byte{1, 1, 0, 0, 1, 0})
				return err
			},
		},
		{
			name: "incoming TPDU",
			run: func() error {
				_, _, err := decodeRuntimeSMSDeliver(context.Background(), []byte{0})
				return err
			},
		},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			err := check.run()
			if err == nil || !strings.Contains(err.Error(), "Rust SMS codec service is unavailable") {
				t.Fatalf("expected fail-closed native codec error, got %v", err)
			}
		})
	}
}

func TestNativeSMSCancelledBeforeEitherBackend(t *testing.T) {
	for _, socket := range []string{"", "/missing/native.sock"} {
		t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := encodeRuntimeRPDU(ctx, 42, "888", []byte{1, 42}); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	}
}

func TestNativeSMSRejectsCorruptEnvelope(t *testing.T) {
	valid := []byte{0, 42, 0, 3, 129, 136, 248, 2, 1, 42}
	if !validNativeRPDU(valid, 42, []byte{1, 42}) {
		t.Fatal("valid envelope rejected")
	}
	for _, index := range []int{0, 1, 2, 3, 7, 8, 9} {
		corrupt := append([]byte(nil), valid...)
		corrupt[index]++
		if validNativeRPDU(corrupt, 42, []byte{1, 42}) {
			t.Fatalf("accepted corruption at %d", index)
		}
	}
	for length := 0; length < len(valid); length++ {
		if validNativeRPDU(valid[:length], 42, []byte{1, 42}) {
			t.Fatalf("accepted truncation %d", length)
		}
	}
	if validNativeRPDU(append(valid, 0), 42, []byte{1, 42}) {
		t.Fatal("accepted trailing octet")
	}
}

func TestNativeSMSUsesOctetArrays(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pc-sms-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	expected := []byte{0, 42, 0, 3, 129, 136, 248, 2, 1, 42}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Reference byte
			SMSC      string
			TPDU      []int
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Reference != 42 || input.SMSC != "888" || !reflect.DeepEqual(input.TPDU, []int{1, 42}) || r.URL.Path != "/api/v2/sms/encode-rpdu" {
			http.Error(w, "bad envelope request", 400)
			return
		}
		_, _ = w.Write([]byte(`{"rpdu":[0,42,0,3,129,136,248,2,1,42]}`))
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	got, err := encodeRuntimeRPDU(context.Background(), 42, "888", []byte{1, 42})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatal("native RPDU bytes changed in JSON bridge")
	}
}

func TestNativeSMSDecodeBridge(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pc-rx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/sms/decode-rpdu" {
			http.Error(w, "wrong route", 400)
			return
		}
		var input struct {
			RPDU []int `json:"rpdu"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !reflect.DeepEqual(input.RPDU, []int{1, 42, 0, 0, 2, 1, 42}) {
			http.Error(w, "invalid request", 400)
			return
		}
		_, _ = w.Write([]byte(`{"reference":42,"originator":null,"tpdu":[1,42]}`))
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	reference, originator, tpdu, err := decodeRuntimeRPDU(context.Background(), []byte{1, 42, 0, 0, 2, 1, 42})
	if err != nil || reference != 42 || originator != "" || !reflect.DeepEqual(tpdu, []byte{1, 42}) {
		t.Fatalf("invalid decoded result: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := decodeRuntimeRPDU(ctx, []byte{1, 42, 0, 0, 2, 1, 42}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
}

func TestNativeSMSDeliverBridge(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pc-deliver-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/sms/decode-incoming-tpdu" {
			http.Error(w, "wrong route", 400)
			return
		}
		var input struct {
			TPDU []int `json:"tpdu"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "invalid body", 400)
			return
		}
		switch {
		case reflect.DeepEqual(input.TPDU, []int{0, 3, 129, 136, 248, 0, 8, 98, 144, 96, 0, 0, 0, 0, 2, 0, 65}):
			_, _ = w.Write([]byte(`{"kind":"deliver","sender":"888","text":"A","binary":null,"service_center_time":{"year":2026,"month":9,"day":6,"hour":12,"minute":0,"second":0,"offset_minutes":480}}`))
		case reflect.DeepEqual(input.TPDU, []int{2, 1, 4, 145, 68, 119, 98, 128, 98, 97, 33, 0, 0, 98, 128, 98, 97, 33, 0, 0, 0}):
			_, _ = w.Write([]byte(`{"kind":"status_report","reference":1,"recipient":"+4477","status_code":0,"delivery_status":"delivered"}`))
		default:
			http.Error(w, "invalid body", 400)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	tpdu := []byte{0, 3, 129, 136, 248, 0, 8, 98, 144, 96, 0, 0, 0, 0, 2, 0, 65}
	decoded, handled, err := decodeRuntimeSMSDeliver(context.Background(), tpdu)
	if err != nil || !handled || decoded.Sender != "888" || decoded.Content != "A" || !decoded.HasTimestamp || decoded.Timestamp.Format(time.RFC3339) != "2026-09-06T12:00:00+08:00" {
		t.Fatalf("invalid native delivery result: %#v %v", decoded, err)
	}
	report := []byte{2, 1, 4, 145, 68, 119, 98, 128, 98, 97, 33, 0, 0, 98, 128, 98, 97, 33, 0, 0, 0}
	decoded, handled, err = decodeRuntimeSMSDeliver(context.Background(), report)
	if err != nil || !handled || !decoded.StatusReport || decoded.DeliveryStatus != "delivered" || decoded.StatusCode != 0 {
		t.Fatalf("invalid native status result: %#v %v", decoded, err)
	}
}

func TestNativeSMSDecodeRejectsPartialPayload(t *testing.T) {
	body := []byte{1, 42, 0, 0, 3, 1, 2, 3}
	if !validNativeIncomingRPDU(body, 42, []byte{1, 2, 3}) {
		t.Fatal("valid frame rejected")
	}
	for _, payload := range [][]byte{nil, {3}, {2, 3}, {1, 2, 3, 4}} {
		if validNativeIncomingRPDU(body, 42, payload) {
			t.Fatal("accepted partial or extended payload")
		}
	}
	for length := 0; length < len(body); length++ {
		if validNativeIncomingRPDU(body[:length], 42, []byte{1, 2, 3}) {
			t.Fatal("accepted truncated frame")
		}
	}
	for _, malformed := range [][]byte{{1, 42, 255, 0, 3, 1, 2, 3}, {1, 42, 0, 255, 3, 1, 2, 3}, {1, 42, 0, 0, 2, 1, 2, 3}} {
		if validNativeIncomingRPDU(malformed, 42, []byte{1, 2, 3}) {
			t.Fatal("accepted invalid framing")
		}
	}
}
