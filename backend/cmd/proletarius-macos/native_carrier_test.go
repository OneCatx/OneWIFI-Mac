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
	"time"

	"proletarius.local/core/internal/vowifi/carrier"
)

func TestRuntimeCarrierHonorsCancellationBeforeLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolveRuntimeCarrierContext(ctx, carrier.SIMIdentity{MCC: "234", MNC: "33"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}

func TestRuntimeCarrierCancelsInFlightRequest(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pc-cancel-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "c.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	started := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() })}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := resolveRuntimeCarrierContext(ctx, carrier.SIMIdentity{MCC: "234", MNC: "33"})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach test server")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled request remained blocked")
	}
}

func TestRuntimeCarrierUsesNativeSocketAndDoesNotFallback(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "pc-carrier-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "carrier.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", socket)
	t.Setenv("PROLETARIUS_APP_TOKEN", "fixture-token")
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Proletarius-App-Token") != "fixture-token" || r.URL.Path != "/api/v2/carrier/resolve" || r.Method != http.MethodPost {
			http.Error(w, "invalid request", http.StatusUnauthorized)
			return
		}
		var identity map[string]string
		if err := json.NewDecoder(r.Body).Decode(&identity); err != nil {
			http.Error(w, "invalid", 400)
			return
		}
		if identity["mnc"] == "10" {
			http.Error(w, "ambiguous", 422)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"matched":  true,
			"revision": "fixture-revision",
			"runtime_profile": carrier.NativeRuntimeProfile{Profile: carrier.Profile{
				ID: "native_fixture", MCC: "234", MNC: "33", APN: "native-ims", IPStack: "ipv6",
				RegistryRevision: "fixture-revision",
			}},
		})
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	p, err := resolveRuntimeCarrier(carrier.SIMIdentity{MCC: "234", MNC: "33"})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "native_fixture" || p.APN != "native-ims" || p.IPStack != "ipv6" || p.RegistryRevision != "fixture-revision" {
		t.Fatalf("native profile mapping failed: %s", p.ID)
	}
	if _, err := resolveRuntimeCarrier(carrier.SIMIdentity{MCC: "234", MNC: "10"}); err == nil {
		t.Fatal("native rejection must not fall back to Go selection")
	}
}

func TestSafeNativeFailureCodeRejectsUntrustedDetail(t *testing.T) {
	for _, test := range []struct {
		code string
		want bool
	}{
		{"reader_authentication_failed", true},
		{"pcsc_sharing_violation", true},
		{"", false},
		{"reader failed: RAND=secret", false},
		{"路径错误", false},
	} {
		if got := safeNativeFailureCode(test.code); got != test.want {
			t.Fatalf("safeNativeFailureCode(%q) = %v, want %v", test.code, got, test.want)
		}
	}
}
